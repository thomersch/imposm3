package main

import (
	"flag"
	"fmt"
	"goposm/cache"
	"goposm/database"
	_ "goposm/database/postgis"
	"goposm/element"
	"goposm/geom"
	"goposm/geom/geos"
	"goposm/mapping"
	"goposm/parser"
	"goposm/proj"
	"goposm/stats"
	"goposm/writer"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"
)

var skipCoords, skipNodes, skipWays bool
var dbImportBatchSize int64

func init() {
	if os.Getenv("GOPOSM_SKIP_COORDS") != "" {
		skipCoords = true
	}
	if os.Getenv("GOPOSM_SKIP_NODES") != "" {
		skipNodes = true
	}
	if os.Getenv("GOPOSM_SKIP_WAYS") != "" {
		skipWays = true
	}

	dbImportBatchSize, _ = strconv.ParseInt(
		os.Getenv("GOPOSM_DBIMPORT_BATCHSIZE"), 10, 32)

	if dbImportBatchSize == 0 {
		dbImportBatchSize = 4096
	}
}

type ErrorLevel interface {
	Level() int
}

func parse(cache *cache.OSMCache, progress *stats.Statistics, tagmapping *mapping.Mapping, filename string) {
	nodes := make(chan []element.Node, 16)
	coords := make(chan []element.Node, 16)
	ways := make(chan []element.Way, 16)
	relations := make(chan []element.Relation, 16)

	positions := parser.PBFBlockPositions(filename)

	waitParser := sync.WaitGroup{}
	for i := 0; i < runtime.NumCPU(); i++ {
		waitParser.Add(1)
		go func() {
			for pos := range positions {
				parser.ParseBlock(
					pos,
					coords,
					nodes,
					ways,
					relations,
				)
			}
			waitParser.Done()
		}()
	}

	waitCounter := sync.WaitGroup{}

	for i := 0; i < runtime.NumCPU(); i++ {
		waitCounter.Add(1)
		go func() {
			m := tagmapping.WayTagFilter()
			for ws := range ways {
				if skipWays {
					continue
				}
				for i, _ := range ws {
					m.Filter(&ws[i].Tags)
				}
				cache.Ways.PutWays(ws)
				progress.AddWays(len(ws))
			}
			waitCounter.Done()
		}()
	}
	for i := 0; i < runtime.NumCPU(); i++ {
		waitCounter.Add(1)
		go func() {
			m := tagmapping.RelationTagFilter()
			for rels := range relations {
				for i, _ := range rels {
					m.Filter(&rels[i].Tags)
				}
				cache.Relations.PutRelations(rels)
				progress.AddRelations(len(rels))
			}
			waitCounter.Done()
		}()
	}
	for i := 0; i < runtime.NumCPU(); i++ {
		waitCounter.Add(1)
		go func() {
			for nds := range coords {
				if skipCoords {
					continue
				}
				cache.Coords.PutCoords(nds)
				progress.AddCoords(len(nds))
			}
			waitCounter.Done()
		}()
	}
	for i := 0; i < 2; i++ {
		waitCounter.Add(1)
		go func() {
			m := tagmapping.NodeTagFilter()
			for nds := range nodes {
				if skipNodes {
					continue
				}
				for i, _ := range nds {
					m.Filter(&nds[i].Tags)
				}
				n, _ := cache.Nodes.PutNodes(nds)
				progress.AddNodes(n)
			}
			waitCounter.Done()
		}()
	}

	waitParser.Wait()
	close(coords)
	close(nodes)
	close(ways)
	close(relations)
	waitCounter.Wait()
}

var (
	cpuprofile     = flag.String("cpuprofile", "", "filename of cpu profile output")
	memprofile     = flag.String("memprofile", "", "dir name of mem profile output and interval (fname:interval)")
	cachedir       = flag.String("cachedir", "/tmp/goposm", "cache directory")
	overwritecache = flag.Bool("overwritecache", false, "overwritecache")
	appendcache    = flag.Bool("appendcache", false, "append cache")
	read           = flag.String("read", "", "read")
	write          = flag.Bool("write", false, "write")
	connection     = flag.String("connection", "", "connection parameters")
	diff           = flag.Bool("diff", false, "enable diff support")
	mappingFile    = flag.String("mapping", "", "mapping file")
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if *memprofile != "" {
		parts := strings.Split(*memprofile, string(os.PathListSeparator))
		var interval time.Duration

		if len(parts) < 2 {
			interval, _ = time.ParseDuration("1m")
		} else {
			var err error
			interval, err = time.ParseDuration(parts[1])
			if err != nil {
				log.Fatal(err)
			}
		}

		go stats.MemProfiler(parts[0], interval)
	}

	osmCache := cache.NewOSMCache(*cachedir)

	if *read != "" && osmCache.Exists() {
		if *overwritecache {
			log.Println("removing existing cache", *cachedir)
			err := osmCache.Remove()
			if err != nil {
				log.Fatal("unable to remove cache:", err)
			}
		} else if !*appendcache {
			log.Fatal("cache already exists use -appendcache or -overwritecache")
		}
	}

	err := osmCache.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer osmCache.Close()

	progress := stats.StatsReporter()

	tagmapping, err := mapping.NewMapping(*mappingFile)
	if err != nil {
		log.Fatal(err)
	}

	if *read != "" {
		osmCache.Coords.SetLinearImport(true)
		parse(osmCache, progress, tagmapping, *read)
		osmCache.Coords.SetLinearImport(false)
		progress.Reset()
		osmCache.Coords.Flush()
	}

	if *write {
		progress.Reset()

		diffCache := cache.NewDiffCache(*cachedir)
		if err = diffCache.Remove(); err != nil {
			log.Fatal(err)
		}
		if err = diffCache.Open(); err != nil {
			log.Fatal(err)
		}

		waitFill := sync.WaitGroup{}
		wayChan := make(chan []element.Way)
		conf := database.Config{
			Type:             "postgis",
			ConnectionParams: *connection,
			Srid:             3857,
		}
		pg, err := database.Open(conf)
		if err != nil {
			log.Fatal(err)
		}

		err = pg.Init(tagmapping)
		if err != nil {
			log.Fatal(err)
		}

		insertBuffer := writer.NewInsertBuffer()
		dbWriter := writer.NewDbWriter(pg, insertBuffer.Out)

		rel := osmCache.Relations.Iter()
		polygons := tagmapping.PolygonMatcher()

		for r := range rel {
			progress.AddRelations(1)
			err := osmCache.Ways.FillMembers(r.Members)
			if err == cache.NotFound {
				// fmt.Println("missing ways for relation", r.Id)
			} else if err != nil {
				fmt.Println(err)
				continue
			}
			for _, m := range r.Members {
				if m.Way == nil {
					continue
				}
				err := osmCache.Coords.FillWay(m.Way)
				if err == cache.NotFound {
					// fmt.Println("missing nodes for way", m.Way.Id, "in relation", r.Id)
				} else if err != nil {
					fmt.Println(err)
					continue
				}
				proj.NodesToMerc(m.Way.Nodes)
			}

			err = geom.BuildRelation(r)
			if err != nil {
				if err, ok := err.(ErrorLevel); ok {
					if err.Level() <= 0 {
						continue
					}
				}
				log.Println(err)
				continue
			}
			if matches := polygons.Match(&r.OSMElem); len(matches) > 0 {
				for _, match := range matches {
					row := match.Row(&r.OSMElem)
					insertBuffer.Insert(match.Table, row)
				}
				err := osmCache.InsertedWays.PutMembers(r.Members)
				if err != nil {
					fmt.Println(err)
				}
			}
		}

		way := osmCache.Ways.Iter()

		for i := 0; i < runtime.NumCPU(); i++ {
			waitFill.Add(1)
			go func() {
				lineStrings := tagmapping.LineStringMatcher()
				polygons := tagmapping.PolygonMatcher()
				geos := geos.NewGEOS()
				defer geos.Finish()

				for w := range way {
					progress.AddWays(1)
					inserted, err := osmCache.InsertedWays.IsInserted(w.Id)
					if err != nil {
						log.Println(err)
						continue
					}
					if inserted {
						continue
					}

					err = osmCache.Coords.FillWay(w)
					if err != nil {
						continue
					}
					proj.NodesToMerc(w.Nodes)
					if matches := lineStrings.Match(&w.OSMElem); len(matches) > 0 {
						// make copy to avoid interference with polygon matches
						way := element.Way(*w)
						way.Geom, err = geom.LineStringWKB(geos, way.Nodes)
						if err != nil {
							if err, ok := err.(ErrorLevel); ok {
								if err.Level() <= 0 {
									continue
								}
							}
							log.Println(err)
							continue
						}
						for _, match := range matches {
							row := match.Row(&way.OSMElem)
							insertBuffer.Insert(match.Table, row)
						}

					}
					if w.IsClosed() {
						if matches := polygons.Match(&w.OSMElem); len(matches) > 0 {
							way := element.Way(*w)
							way.Geom, err = geom.PolygonWKB(geos, way.Nodes)
							if err != nil {
								if err, ok := err.(ErrorLevel); ok {
									if err.Level() <= 0 {
										continue
									}
								}
								log.Println(err)
								continue
							}
							for _, match := range matches {
								row := match.Row(&way.OSMElem)
								insertBuffer.Insert(match.Table, row)
							}
						}
					}

					if *diff {
						diffCache.Coords.AddFromWay(w)
					}
				}
				waitFill.Done()
			}()
		}
		waitFill.Wait()
		close(wayChan)
		diffCache.Coords.Close()

		nodes := osmCache.Nodes.Iter()
		points := tagmapping.PointMatcher()
		geos := geos.NewGEOS()
		defer geos.Finish()
		for n := range nodes {
			progress.AddNodes(1)
			if matches := points.Match(&n.OSMElem); len(matches) > 0 {
				proj.NodeToMerc(n)
				n.Geom, err = geom.PointWKB(geos, *n)
				if err != nil {
					if err, ok := err.(ErrorLevel); ok {
						if err.Level() <= 0 {
							continue
						}
					}
					log.Println(err)
					continue
				}
				for _, match := range matches {
					row := match.Row(&n.OSMElem)
					insertBuffer.Insert(match.Table, row)
				}

			}
			// fmt.Println(r)
		}
		insertBuffer.Close()
		dbWriter.Close()

	}
	progress.Stop()

	//parser.PBFStats(os.Args[1])
}
