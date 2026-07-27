package main

import (
	"image/color"
	"math"
	"sort"
	"strconv"
	"strings"

	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/plotutil"
	"gonum.org/v1/plot/vg"
	"gonum.org/v1/plot/vg/draw"

	"github.com/soypat/httpbench/internal/report"
)

// Figure geometry. Sizes derive from the height so a wider figure does not
// shrink its text.
const (
	plotHeight = 6 * vg.Inch
	plotWidth  = 10 * vg.Inch
	fontsize   = plotHeight / 28
)

// drawer keeps what has to stay the same across figures: a server drawn in one
// colour in the first figure is drawn in that colour in all of them, so a reader
// who learned the legend once does not have to learn it again.
type drawer struct {
	colors map[string]color.Color
}

// newDrawer assigns a colour per implementation and transport, in the order the
// report measured them. Nothing here knows the name of any implementation: one
// added later gets a colour without this file being touched.
func newDrawer(rep *report.Report) *drawer {
	d := &drawer{colors: make(map[string]color.Color)}
	for _, r := range rep.Results {
		key := r.Impl + "/" + r.Transport
		if _, ok := d.colors[key]; !ok {
			d.colors[key] = plotutil.Color(len(d.colors))
		}
	}
	return d
}

func (d *drawer) color(impl, transport string) color.Color {
	if c, ok := d.colors[impl+"/"+transport]; ok {
		return c
	}
	return color.Gray{Y: 120}
}

// series is one line across a figure's X axis: a server, over a transport, in
// one connection mode.
type series struct {
	impl      string
	transport string
	mode      string
	points    plotter.XYs
}

func (s series) name() string { return report.SeriesName(s.impl, s.transport, s.mode) }

func seriesShape(mode string) draw.GlyphDrawer {
	if mode == report.ModeKeepAlive {
		return draw.TriangleGlyph{}
	}
	return draw.CircleGlyph{}
}

// addSeries draws every series as a line with a glyph per point and fills the
// legend. Lines are what a log axis can show; bars would start at zero.
func (d *drawer) addSeries(p *plot.Plot, all []series) error {
	for _, s := range all {
		line, points, err := plotter.NewLinePoints(s.points)
		if err != nil {
			return err
		}
		c := d.color(s.impl, s.transport)
		line.Color = c
		line.Width = vg.Points(1.5)
		points.Shape = seriesShape(s.mode)
		points.Color = c
		points.Radius = vg.Points(3)
		if s.mode == report.ModeKeepAlive {
			line.Dashes = []vg.Length{vg.Points(4), vg.Points(2)}
		}
		p.Add(line, points)
		p.Legend.Add(s.name(), line, points)
	}
	return nil
}

// newPlot returns a figure with the sizing and fonts shared by all of them.
func newPlot(title, xLabel, yLabel string) *plot.Plot {
	p := plot.New()
	p.Title.Text = title
	p.X.Label.Text = xLabel
	p.Y.Label.Text = yLabel
	p.Title.TextStyle.Font.Size = fontsize * 1.2
	p.Legend.TextStyle.Font.Size = fontsize * 0.85
	p.X.Label.TextStyle.Font.Size = fontsize
	p.Y.Label.TextStyle.Font.Size = fontsize
	p.X.Tick.Label.Font.Size = fontsize * 0.8
	p.Y.Tick.Label.Font.Size = fontsize * 0.8
	p.Legend.Top = true
	p.Legend.Left = true
	p.Add(plotter.NewGrid())
	return p
}

// logY puts the Y axis on a log scale: a figure holding both 25 B and 29 kB has
// nothing to show on a linear one.
func logY(p *plot.Plot, marker plot.Ticker) {
	p.Y.Scale = plot.LogScale{}
	p.Y.Tick.Marker = marker
}

// workloadRows are the per-scenario measurements: not the concurrency sweep, not
// the flood probe, not the static or idle rows.
func workloadRows(rep *report.Report) []report.Result {
	var out []report.Result
	for _, r := range rep.Results {
		if r.Kind != "" || strings.Contains(r.Scenario, "@") || strings.HasPrefix(r.Scenario, "flood/") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// scenarioOrder returns the workloads in the order they were measured, which
// runs from the cheapest request to the most expensive one.
func scenarioOrder(rows []report.Result) []string {
	var order []string
	for _, r := range rows {
		if indexOf(order, r.Scenario) < 0 {
			order = append(order, r.Scenario)
		}
	}
	return order
}

// groupSeries turns rows into one line per server, transport and mode, with a
// point per scenario in the given order. value picks what is plotted.
func groupSeries(rows []report.Result, order []string, value func(report.Result) float64) []series {
	var out []series
	for _, r := range rows {
		v := value(r)
		if v <= 0 {
			continue // Log axes have nothing to say about zero.
		}
		x := indexOf(order, r.Scenario)
		if x < 0 {
			continue
		}
		i := seriesIndex(out, r.Impl, r.Transport, r.Mode)
		if i < 0 {
			out = append(out, series{impl: r.Impl, transport: r.Transport, mode: r.Mode})
			i = len(out) - 1
		}
		out[i].points = append(out[i].points, plotter.XY{X: float64(x), Y: v})
	}
	sortPoints(out)
	return out
}

func sortPoints(all []series) {
	for i := range all {
		pts := all[i].points
		sort.Slice(pts, func(a, b int) bool { return pts[a].X < pts[b].X })
	}
}

func seriesIndex(all []series, impl, transport, mode string) int {
	for i := range all {
		if all[i].impl == impl && all[i].transport == transport && all[i].mode == mode {
			return i
		}
	}
	return -1
}

func indexOf(s []string, v string) int {
	for i := range s {
		if s[i] == v {
			return i
		}
	}
	return -1
}

// zeroSeriesNames names the servers a log axis cannot draw, because they
// measured zero or below everywhere. A missing line is the strongest result in
// these figures, so it is written on them rather than left out.
func zeroSeriesNames(rows []report.Result, value func(report.Result) float64) []string {
	var names, nonzero []string
	for _, r := range rows {
		name := report.SeriesName(r.Impl, r.Transport, r.Mode)
		if value(r) > 0 {
			nonzero = append(nonzero, name)
		} else if indexOf(names, name) < 0 {
			names = append(names, name)
		}
	}
	var out []string
	for _, name := range names {
		if indexOf(nonzero, name) < 0 {
			out = append(out, name) // At or below zero in every row it appeared in.
		}
	}
	return out
}

// noteZeros writes the names of those servers along the bottom of a figure, at
// the axis minimum, where their line would have run.
func (d *drawer) noteZeros(p *plot.Plot, names []string, what string) error {
	if len(names) == 0 {
		return nil
	}
	labels, err := plotter.NewLabels(plotter.XYLabels{
		XYs:    plotter.XYs{{X: p.X.Min, Y: p.Y.Min}},
		Labels: []string{strings.Join(names, ", ") + ": " + what},
	})
	if err != nil {
		return err
	}
	labels.TextStyle[0].Font.Size = fontsize * 0.9
	labels.TextStyle[0].XAlign = draw.XLeft
	labels.TextStyle[0].YAlign = draw.YBottom
	p.Add(labels)
	return nil
}

// padLogAxis widens a log axis out to the 1-2-5 steps around the data, so the
// lowest series gets a tick under it instead of floating above the axis.
func padLogAxis(axis *plot.Axis, all []series, value func(plotter.XY) float64) {
	min, max := math.Inf(1), math.Inf(-1)
	for _, s := range all {
		for _, pt := range s.points {
			v := value(pt)
			if v <= 0 {
				continue
			}
			min = math.Min(min, v)
			max = math.Max(max, v)
		}
	}
	if math.IsInf(min, 1) {
		return
	}
	axis.Min = stepBelow(min)
	axis.Max = stepAbove(max)
}

// stepBelow and stepAbove return the nearest 1-2-5 step of a decade on either
// side of v, which is where [logTicks] puts its labels.
func stepBelow(v float64) float64 {
	decade := math.Pow(10, math.Floor(math.Log10(v)))
	for _, step := range []float64{5, 2, 1} {
		if s := decade * step; s <= v {
			return s
		}
	}
	return decade / 2
}

func stepAbove(v float64) float64 {
	decade := math.Pow(10, math.Floor(math.Log10(v)))
	for _, step := range []float64{1, 2, 5, 10} {
		if s := decade * step; s >= v {
			return s
		}
	}
	return decade * 10
}

// logTicks walks the 1-2-5 steps of every decade the axis spans, labelling each
// with label. A decade starting below the axis still contributes its steps,
// which is what puts a tick next to a series sitting at 25 bytes.
func logTicks(min, max float64, label func(float64) string) []plot.Tick {
	if min <= 0 || max <= min {
		return nil
	}
	var ticks []plot.Tick
	decade := math.Pow(10, math.Floor(math.Log10(min)))
	for ; decade <= max; decade *= 10 {
		for _, step := range []float64{1, 2, 5} {
			v := decade * step
			if v < min || v > max {
				continue
			}
			ticks = append(ticks, plot.Tick{Value: v, Label: label(v)})
		}
	}
	return ticks
}

// byteTicks labels a log axis in bytes. Steps are decimal, so is the label: a
// tick at 2000 reads 2 kB rather than 1.95 kB.
type byteTicks struct{}

func (byteTicks) Ticks(min, max float64) []plot.Tick {
	return logTicks(min, max, decimalBytes)
}

// countTicks labels a log axis in plain counts.
type countTicks struct{}

func (countTicks) Ticks(min, max float64) []plot.Tick {
	return logTicks(min, max, trimFloat)
}

// byteTicksLinear labels a linear axis in bytes.
type byteTicksLinear struct{}

func (byteTicksLinear) Ticks(min, max float64) []plot.Tick {
	ticks := plot.DefaultTicks{}.Ticks(min, max)
	for i := range ticks {
		if ticks[i].Label != "" {
			ticks[i].Label = decimalBytes(ticks[i].Value)
		}
	}
	return ticks
}

// countTicksLinear labels a linear axis in thousands, so a throughput axis reads
// 225k rather than 225000.
type countTicksLinear struct{}

func (countTicksLinear) Ticks(min, max float64) []plot.Tick {
	ticks := plot.DefaultTicks{}.Ticks(min, max)
	for i := range ticks {
		if ticks[i].Label != "" {
			ticks[i].Label = humanCount(ticks[i].Value)
		}
	}
	return ticks
}

func decimalBytes(v float64) string {
	switch {
	case v < 0:
		return "-" + decimalBytes(-v)
	case v >= 1e6:
		return trimFloat(v/1e6) + " MB"
	case v >= 1e3:
		return trimFloat(v/1e3) + " kB"
	}
	return trimFloat(v) + " B"
}

func humanCount(v float64) string {
	switch {
	case v >= 1e6:
		return trimFloat(v/1e6) + "M"
	case v >= 1000:
		return trimFloat(v/1000) + "k"
	}
	return trimFloat(v)
}

func trimFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

// nominalX labels the X axis with the workload names, keeping the axis numeric
// so the lines still connect across it.
func nominalX(p *plot.Plot, names []string) {
	ticks := make([]plot.Tick, len(names))
	for i, name := range names {
		ticks[i] = plot.Tick{Value: float64(i), Label: name}
	}
	p.X.Tick.Marker = plot.ConstantTicks(ticks)
	p.X.Min = -0.3
	p.X.Max = float64(len(names)) - 0.7
}

// drawPerRequest is the headline: what one answer costs the server that gave it.
func (d *drawer) drawPerRequest(rep *report.Report, path string) error {
	rows := workloadRows(rep)
	if len(rows) == 0 {
		return errNoRows
	}
	heap := func(r report.Result) float64 { return r.HeapPerReq }
	order := scenarioOrder(rows)
	all := groupSeries(rows, order, heap)
	p := newPlot("Heap allocated per request (log scale, lower is better)",
		"Workload", "Bytes allocated per request")
	logY(p, byteTicks{})
	if err := d.addSeries(p, all); err != nil {
		return err
	}
	padLogAxis(&p.Y, all, func(pt plotter.XY) float64 { return pt.Y })
	nominalX(p, order)
	// A server measured at or below zero has no line on a log axis, and that is
	// the result: it allocated nothing a sample could tell from noise.
	if err := d.noteZeros(p, zeroSeriesNames(rows, heap), "at the noise floor, every workload"); err != nil {
		return err
	}
	return p.Save(plotWidth, plotHeight, path)
}

// drawFlood puts what a server spends against what it was sent, which is where a
// parser working in memory it took up front and one allocating per field stop
// resembling each other.
func (d *drawer) drawFlood(rep *report.Report, path string) error {
	var rows []report.Result
	for _, r := range rep.Results {
		if r.Kind == "" && strings.HasPrefix(r.Scenario, "flood/") {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return errNoRows
	}
	var all []series
	for _, r := range rows {
		if r.WireBytes <= 0 || r.HeapPerReq <= 0 {
			continue
		}
		i := seriesIndex(all, r.Impl, r.Transport, r.Mode)
		if i < 0 {
			all = append(all, series{impl: r.Impl, transport: r.Transport, mode: r.Mode})
			i = len(all) - 1
		}
		all[i].points = append(all[i].points, plotter.XY{X: float64(r.WireBytes), Y: r.HeapPerReq})
	}
	sortPoints(all)
	p := newPlot("Header flood: heap the server spends on one request (log/log)",
		"Header block sent, bytes on the wire", "Bytes allocated per request")
	p.X.Scale = plot.LogScale{}
	p.X.Tick.Marker = byteTicks{}
	logY(p, byteTicks{})
	if err := d.addSeries(p, all); err != nil {
		return err
	}
	// The line a server that spends exactly what it was sent would follow.
	if err := addUnityLine(p, all); err != nil {
		return err
	}
	padLogAxis(&p.X, all, func(pt plotter.XY) float64 { return pt.X })
	padLogAxis(&p.Y, all, func(pt plotter.XY) float64 { return pt.Y })
	zeros := zeroSeriesNames(rows, func(r report.Result) float64 { return r.HeapPerReq })
	if err := d.noteZeros(p, zeros, "at the noise floor, at every header block size"); err != nil {
		return err
	}
	return p.Save(plotWidth, plotHeight, path)
}

// addUnityLine marks "one byte of heap per byte on the wire", the reference a
// reader needs to see whether a server amplifies its input or bounds it.
func addUnityLine(p *plot.Plot, all []series) error {
	min, max := math.Inf(1), math.Inf(-1)
	for _, s := range all {
		for _, pt := range s.points {
			min = math.Min(min, pt.X)
			max = math.Max(max, pt.X)
		}
	}
	if math.IsInf(min, 1) {
		return nil
	}
	line, err := plotter.NewLine(plotter.XYs{{X: min, Y: min}, {X: max, Y: max}})
	if err != nil {
		return err
	}
	line.Color = color.Gray{Y: 160}
	line.Dashes = []vg.Length{vg.Points(2), vg.Points(3)}
	p.Add(line)
	p.Legend.Add("wire size (1x)", line)
	return nil
}

// concurrencySeries reads the "@N" rows of the sweep.
func concurrencySeries(rep *report.Report, value func(report.Result) float64) ([]series, []int) {
	var conns []int
	var all []series
	for _, r := range rep.Results {
		if r.Kind != "" || !strings.Contains(r.Scenario, "@") {
			continue
		}
		v := value(r)
		if v <= 0 {
			continue
		}
		if !containsInt(conns, r.Conns) {
			conns = append(conns, r.Conns)
		}
		i := seriesIndex(all, r.Impl, r.Transport, r.Mode)
		if i < 0 {
			all = append(all, series{impl: r.Impl, transport: r.Transport, mode: r.Mode})
			i = len(all) - 1
		}
		all[i].points = append(all[i].points, plotter.XY{X: float64(r.Conns), Y: v})
	}
	sort.Ints(conns)
	sortPoints(all)
	return all, conns
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// connTicks labels the sweep at the connection counts it was run at.
func connTicks(conns []int) plot.ConstantTicks {
	ticks := make([]plot.Tick, len(conns))
	for i, c := range conns {
		ticks[i] = plot.Tick{Value: float64(c), Label: strconv.Itoa(c)}
	}
	return plot.ConstantTicks(ticks)
}

func (d *drawer) drawThroughput(rep *report.Report, path string) error {
	all, conns := concurrencySeries(rep, func(r report.Result) float64 { return r.Throughput })
	if len(all) == 0 {
		return errNoRows
	}
	p := newPlot("Throughput against open connections",
		"Connections in flight", "Requests per second")
	p.X.Scale = plot.LogScale{}
	p.X.Tick.Marker = connTicks(conns)
	p.Y.Tick.Marker = countTicksLinear{}
	if err := d.addSeries(p, all); err != nil {
		return err
	}
	return p.Save(plotWidth, plotHeight, path)
}

func (d *drawer) drawLatency(rep *report.Report, path string) error {
	all, conns := concurrencySeries(rep, func(r report.Result) float64 { return float64(r.P99Nanos) / 1e6 })
	if len(all) == 0 {
		return errNoRows
	}
	p := newPlot("Tail latency against open connections (log scale, lower is better)",
		"Connections in flight", "p99 latency, milliseconds")
	p.X.Scale = plot.LogScale{}
	p.X.Tick.Marker = connTicks(conns)
	logY(p, countTicks{})
	if err := d.addSeries(p, all); err != nil {
		return err
	}
	return p.Save(plotWidth, plotHeight, path)
}
