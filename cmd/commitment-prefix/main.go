// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/components"
	"github.com/go-echarts/go-echarts/v2/opts"
	"github.com/go-echarts/go-echarts/v2/types"
	"github.com/schollz/progressbar/v3"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/erigontech/erigon-lib/commitment"
	"github.com/erigontech/erigon-lib/seg"
	"github.com/erigontech/erigon-lib/state"
)

var (
	flagOutputDirectory = flag.String("output", "", "existing directory to store output images. By default, same as commitment files")
	flagConcurrency     = flag.Int("j", 4, "amount of concurrently proceeded files")
	flagTrieVariant     = flag.String("trie", "hex", "commitment trie variant (values are hex and bin)")
	flagCompression     = flag.String("compression", "none", "compression type (none, k, v, kv)")
	flagDepth           = flag.Int("depth", 0, "depth of the prefixes to analyze")
)

func main() {
	flag.Parse()
	if len(os.Args) == 1 {
		fmt.Printf("no .kv file path provided")
		return
	}

	proceedFiles(flag.Args())
}

func proceedFiles(files []string) {
	sema := make(chan struct{}, *flagConcurrency)

	prog := make([]*progressbar.ProgressBar, len(files))
	mapping := make(map[string]int)
	for i := 0; i < cap(sema); i++ {
		sema <- struct{}{}
	}
	for i := 0; i < len(files); i++ {
		fb := filepath.Base(files[i])
		prog[i] = progressbar.DefaultBytes(0, fb)
		mapping[fb] = i
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	page := components.NewPage()
	page.SetLayout(components.PageFlexLayout)
	page.PageTitle = "Commitment Analysis"

	mpg := NewMultiProgressBar(prog, os.Stdout)
	defer mpg.End()

	for i, fp := range files {
		fpath, pos := fp, i
		_ = pos
		<-sema

		//fmt.Printf("\r[%d/%d] - %s..", pos+1, len(files), path.Base(fpath))

		wg.Add(1)
		go func(wg *sync.WaitGroup, mu *sync.Mutex) {
			defer wg.Done()
			defer func() { sema <- struct{}{} }()

			stat, err := processCommitmentFile(fpath, prog[mapping[filepath.Base(fpath)]])
			if err != nil {
				fmt.Printf("processing failed: %v", err)
				return
			}

			mu.Lock()
			page.AddCharts(
				prefixLenCountChart(fpath, stat),
				countersChart(fpath, stat),
				mediansChart(fpath, stat),
				fileContentsMapChart(fpath, stat),
			)
			mu.Unlock()
		}(&wg, &mu)
	}
	wg.Wait()
	fmt.Println()

	dir := filepath.Dir(files[0])
	if *flagOutputDirectory != "" {
		dir = *flagOutputDirectory
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		err := os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			panic(err)
		}
	}
	outPath := path.Join(dir, "analysis.html")
	fmt.Printf("rendering total graph to %s\n", outPath)

	f, err := os.Create(outPath)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	defer f.Sync()

	if err := page.Render(io.MultiWriter(f)); err != nil {
		panic(err)
	}
}

type overallStat struct {
	branches   *commitment.BranchStat
	roots      *commitment.BranchStat
	prefixes   map[uint64]*commitment.BranchStat
	prefCount  map[uint64]uint64
	rootsCount uint64
}

func newOverallStat() *overallStat {
	return &overallStat{
		branches:  new(commitment.BranchStat),
		roots:     new(commitment.BranchStat),
		prefixes:  make(map[uint64]*commitment.BranchStat),
		prefCount: make(map[uint64]uint64),
	}
}

func (s *overallStat) Collect(other *overallStat) {
	if other == nil {
		return
	}
	s.branches.Collect(other.branches)
	if other.roots != nil {
		s.roots.Collect(other.roots)
	}
	if other.prefCount != nil {
		for k, v := range other.prefCount {
			s.prefCount[k] += v
		}
	}
	if other.prefixes != nil {
		for k, v := range other.prefixes {
			ps, ok := s.prefixes[k]
			if !ok {
				s.prefixes[k] = v
				continue
			}
			ps.Collect(v)
		}
	}
}

func extractKVPairFromCompressed(filename string, keysSink chan commitment.BranchStat, pg *progressbar.ProgressBar) error {
	defer close(keysSink)
	dec, err := seg.NewDecompressor(filename)
	if err != nil {
		return fmt.Errorf("failed to create decompressor: %w", err)
	}
	defer dec.Close()
	tv := commitment.ParseTrieVariant(*flagTrieVariant)

	fc, err := state.ParseFileCompression(*flagCompression)
	if err != nil {
		return err
	}
	size := dec.Size()
	//paris := dec.Count() / 2
	//cpair := 0
	depth := *flagDepth
	//var afterValPos uint64
	var key, val []byte
	getter := state.NewArchiveGetter(dec.MakeGetter(), fc)

	pg.ChangeMax64(size)
	defer pg.Close()

	for getter.HasNext() {
		key, _ = getter.Next(key[:0])
		if !getter.HasNext() {
			return errors.New("invalid key/value pair during decompression")
		}
		val, _ = getter.Next(val[:0])
		//cpair++

		//if cpair%100000 == 0 {
		//	fmt.Printf("\r%s pair %d/%d %s/%s", filename, cpair, paris,
		//		datasize.ByteSize(afterValPos).HumanReadable(), datasize.ByteSize(size).HumanReadable())
		//}
		pg.Add(len(key) + len(val))

		if depth > len(key) {
			continue
		}
		stat := commitment.DecodeBranchAndCollectStat(key, val, tv)
		if stat == nil {
			fmt.Printf("failed to decode branch: %x %x\n", key, val)
		}
		keysSink <- *stat
	}
	return nil
}

func processCommitmentFile(fpath string, pg *progressbar.ProgressBar) (*overallStat, error) {
	stats := make(chan commitment.BranchStat, 8)
	errch := make(chan error)
	go func() {
		err := extractKVPairFromCompressed(fpath, stats, pg)
		if err != nil {
			errch <- err
		}
		close(errch)
	}()

	totals := newOverallStat()
	for s := range stats {
		if s.IsRoot {
			totals.rootsCount++
			totals.roots.Collect(&s)
		} else {
			totals.branches.Collect(&s)
		}
		totals.prefCount[s.KeySize]++

		ps, ok := totals.prefixes[s.KeySize]
		if !ok {
			ps = new(commitment.BranchStat)
		}
		ps.Collect(&s)
		totals.prefixes[s.KeySize] = ps
	}

	select {
	case err := <-errch:
		if err != nil {
			return nil, err
		}
	default:
	}
	return totals, nil
}

func prefixLenCountChart(fname string, data *overallStat) *charts.Pie {
	items := make([]opts.PieData, 0)
	for prefSize, count := range data.prefCount {
		items = append(items, opts.PieData{Name: strconv.FormatUint(prefSize, 10), Value: count})
	}

	pie := charts.NewPie()
	pie.SetGlobalOptions(
		charts.WithTooltipOpts(opts.Tooltip{Show: true}),
		charts.WithTitleOpts(opts.Title{Subtitle: fname, Title: "key prefix length distribution (bytes)", Top: "25"}),
	)

	pie.AddSeries("prefixLen/count", items)
	return pie
}

func fileContentsMapChart(fileName string, data *overallStat) *charts.TreeMap {
	var TreeMap = []opts.TreeMapNode{
		{Name: "prefixes"},
		{Name: "values"},
	}

	keysIndex := 0
	TreeMap[keysIndex].Children = make([]opts.TreeMapNode, 0)
	for prefSize, stat := range data.prefixes {
		TreeMap[keysIndex].Children = append(TreeMap[keysIndex].Children, opts.TreeMapNode{
			Name:  strconv.FormatUint(prefSize, 10),
			Value: int(stat.KeySize),
		})
	}

	valsIndex := 1
	TreeMap[valsIndex].Children = []opts.TreeMapNode{
		{
			Name:  "hashes",
			Value: int(data.branches.HashSize),
		},
		{
			Name:  "extensions",
			Value: int(data.branches.ExtSize),
		},
		{
			Name:  "accountKey",
			Value: int(data.branches.APKSize),
		},
		{
			Name:  "storageKey",
			Value: int(data.branches.SPKSize),
		},
		{
			Name:  "leafHashes",
			Value: int(data.branches.LeafHashSize),
		},
	}

	graph := charts.NewTreeMap()
	graph.SetGlobalOptions(
		charts.WithInitializationOpts(opts.Initialization{Theme: types.ThemeMacarons}),
		charts.WithLegendOpts(opts.Legend{Show: false}),
		charts.WithTooltipOpts(opts.Tooltip{
			Show:      true,
			Formatter: opts.FuncOpts(ToolTipFormatter),
		}),
	)

	// Add initialized data to graph.
	graph.AddSeries(fileName, TreeMap).
		SetSeriesOptions(
			charts.WithTreeMapOpts(
				opts.TreeMapChart{
					UpperLabel: &opts.UpperLabel{Show: true, Color: "#fff"},
					Levels: &[]opts.TreeMapLevel{
						{ // Series
							ItemStyle: &opts.ItemStyle{
								BorderColor: "#777",
								BorderWidth: 1,
								GapWidth:    1},
							UpperLabel: &opts.UpperLabel{Show: true},
						},
						{ // Level
							ItemStyle: &opts.ItemStyle{
								BorderColor: "#666",
								BorderWidth: 1,
								GapWidth:    1},
							Emphasis: &opts.Emphasis{
								ItemStyle: &opts.ItemStyle{BorderColor: "#555"},
							},
						},
						{ // Node
							ColorSaturation: []float32{0.35, 0.5},
							ItemStyle: &opts.ItemStyle{
								GapWidth:              1,
								BorderWidth:           0,
								BorderColorSaturation: 0.6,
							},
						},
					},
				},
			),
			charts.WithItemStyleOpts(opts.ItemStyle{BorderColor: "#fff"}),
			charts.WithLabelOpts(opts.Label{Show: true, Position: "inside", Color: "White"}),
		)
	return graph
}

var ToolTipFormatter = `
function (info) {
    var bytes = Number(info.value);
    const KB = 1024;
    const MB = 1024 * KB;
    const GB = 1024 * MB;

    let result;
    if (bytes >= GB) {
        result = (bytes / GB).toFixed(2) + ' GB';
    } else if (bytes >= MB) {
        result = (bytes / MB).toFixed(2) + ' MB';
    } else if (bytes >= KB) {
        result = (bytes / KB).toFixed(2) + ' KB';
    } else {
        result = bytes + ' bytes';
    }

    var formatUtil = echarts.format;
    var treePathInfo = info.treePathInfo;
    var treePath = [];
    for (var i = 1; i < treePathInfo.length; i++) {
        treePath.push(treePathInfo[i].name);
    }
    
    return [
        '<div class="tooltip-title" style="color: white;">' + formatUtil.encodeHTML(treePath.join('/')) + '</div>',
				'<span style="color: white;">Disk Usage: ' + result + '</span>',
    ].join('');
}
`

func countersChart(fname string, data *overallStat) *charts.Sankey {
	sankey := charts.NewSankey()
	sankey.SetGlobalOptions(
		charts.WithLegendOpts(opts.Legend{Show: true}),
		charts.WithTooltipOpts(opts.Tooltip{Show: true}),
	)

	nodes := []opts.SankeyNode{
		{Name: "Cells"},
		{Name: "APK"},
		{Name: "SPK"},
		{Name: "Hashes"},
		{Name: "Extensions"},
		{Name: "LeafHashes"},
	}
	sankeyLink := []opts.SankeyLink{
		{Source: nodes[0].Name, Target: nodes[1].Name, Value: float32(data.branches.APKCount)},
		{Source: nodes[0].Name, Target: nodes[2].Name, Value: float32(data.branches.SPKCount)},
		{Source: nodes[0].Name, Target: nodes[3].Name, Value: float32(data.branches.HashCount)},
		{Source: nodes[0].Name, Target: nodes[4].Name, Value: float32(data.branches.ExtCount)},
		{Source: nodes[0].Name, Target: nodes[5].Name, Value: float32(data.branches.LeafHashCount)},
	}

	sankey.AddSeries("Counts "+filepath.Base(fname), nodes, sankeyLink).
		SetSeriesOptions(
			charts.WithLineStyleOpts(opts.LineStyle{
				Color:     "source",
				Curveness: 0.5,
			}),
			charts.WithLabelOpts(opts.Label{
				Show: true,
			}),
		)
	return sankey
}

func mediansChart(fname string, data *overallStat) *charts.Sankey {
	sankey := charts.NewSankey()
	sankey.SetGlobalOptions(
		charts.WithLegendOpts(opts.Legend{Show: true}),
		charts.WithTooltipOpts(opts.Tooltip{Show: true}),
	)

	nodes := []opts.SankeyNode{
		{Name: "Cells"},
		{Name: "Addr"},
		{Name: "Addr+Storage"},
		{Name: "Hashes"},
		{Name: "Extensions"},
		{Name: "LeafHashes"},
	}
	sankeyLink := []opts.SankeyLink{
		{Source: nodes[0].Name, Target: nodes[1].Name, Value: float32(data.branches.MedianAPK)},
		{Source: nodes[0].Name, Target: nodes[2].Name, Value: float32(data.branches.MedianSPK)},
		{Source: nodes[0].Name, Target: nodes[3].Name, Value: float32(data.branches.MedianHash)},
		{Source: nodes[0].Name, Target: nodes[4].Name, Value: float32(data.branches.MedianExt)},
		{Source: nodes[0].Name, Target: nodes[5].Name, Value: float32(data.branches.MedianLH)},
	}

	sankey.AddSeries("Medians "+filepath.Base(fname), nodes, sankeyLink).
		SetSeriesOptions(
			charts.WithLineStyleOpts(opts.LineStyle{
				Color:     "source",
				Curveness: 0.5,
			}),
			charts.WithLabelOpts(opts.Label{
				Show: true,
			}),
		)
	return sankey
}

type LineWriter struct {
	*MultiProgressBar
	id int
}

func (lw *LineWriter) Write(p []byte) (n int, err error) {
	lw.guard.Lock()
	defer lw.guard.Unlock()
	lw.move(lw.id, lw.output)
	return lw.output.Write(p)
}

type MultiProgressBar struct {
	output  io.Writer
	curLine int
	Bars    []*progressbar.ProgressBar
	guard   sync.Mutex
}

func NewMultiProgressBar(pBars []*progressbar.ProgressBar, output io.Writer) *MultiProgressBar {
	mpb := &MultiProgressBar{
		curLine: 0,
		Bars:    pBars,
		guard:   sync.Mutex{},
		output:  output,
	}
	for id, pb := range mpb.Bars {
		progressbar.OptionSetWriter(&LineWriter{
			MultiProgressBar: mpb,
			id:               id,
		})(pb)
	}

	return mpb
}

func (mpb *MultiProgressBar) move(id int, writer io.Writer) (int, error) {
	bias := mpb.curLine - id
	mpb.curLine = id
	if bias > 0 {
		// move up
		return fmt.Fprintf(writer, "\r\033[%dA", bias)
	} else if bias < 0 {
		// move down
		return fmt.Fprintf(writer, "\r\033[%dB", -bias)
	}
	return 0, nil
}

func (mpb *MultiProgressBar) End() {
	mpb.move(len(mpb.Bars), mpb.output)
}
