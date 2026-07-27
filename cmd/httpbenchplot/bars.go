package main

import (
	"image/color"
	"math"
	"sort"

	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
	"gonum.org/v1/plot/vg/draw"

	"github.com/soypat/httpbench/internal/report"
)

// drawPerRequest is the headline: what one answer costs the server that gave it.
//
// Bars, not lines. The X axis is nominal — "hello", "echo/64" and "echo/1024"
// are three workloads, not three positions — and a line drawn between them
// claims an interpolation that does not exist. Bars also have somewhere to put a
// measurement of zero, which a log axis does not: the server whose whole claim
// is that it allocates nothing per request is exactly the one a log axis drops.
//
// The axis is linear, so a bar's height is its value. The spread between
// implementations is three orders of magnitude, which here means one bar at full
// height and another that is a hairline above the axis. That is the result, so it
// is drawn rather than compressed away, and every bar carries its value for the
// ones too short to read off the axis.
func (d *drawer) drawPerRequest(rep *report.Report, path string) error {
	rows := workloadRows(rep)
	if len(rows) == 0 {
		return errNoRows
	}
	order := scenarioOrder(rows)
	groups := gatherBars(rows, order, func(r report.Result) float64 { return r.HeapPerReq })
	if len(groups) == 0 {
		return errNoRows
	}
	p := newPlot("Heap allocated per request (lower is better)",
		"Workload", "Bytes allocated per request")
	if err := d.addBars(p, groups, order, decimalBytes); err != nil {
		return err
	}
	return p.Save(plotWidth, plotHeight, path)
}

// bars is one server's value in every workload, in the order of the X axis. A
// workload it was not measured in holds NaN and is left blank rather than drawn
// as a zero it never measured.
type bars struct {
	impl      string
	transport string
	mode      string
	values    plotter.Values
}

func (b bars) name() string { return report.SeriesName(b.impl, b.transport, b.mode) }

// gatherBars turns rows into one group of bars per server, transport and mode.
// Unlike the line figures nothing is dropped for being zero or negative: a
// measurement at the noise floor is a result this figure can show.
func gatherBars(rows []report.Result, order []string, value func(report.Result) float64) []bars {
	var out []bars
	for _, r := range rows {
		x := indexOf(order, r.Scenario)
		if x < 0 {
			continue
		}
		i := -1
		for j := range out {
			if out[j].impl == r.Impl && out[j].transport == r.Transport && out[j].mode == r.Mode {
				i = j
				break
			}
		}
		if i < 0 {
			blank := make(plotter.Values, len(order))
			for j := range blank {
				blank[j] = math.NaN()
			}
			out = append(out, bars{impl: r.Impl, transport: r.Transport, mode: r.Mode, values: blank})
			i = len(out) - 1
		}
		out[i].values[x] = value(r)
	}
	// One connection per request first, keep-alive after it, so the two modes of
	// the same server stand next to each other in the order they are discussed.
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].impl != out[b].impl {
			return out[a].impl < out[b].impl
		}
		if out[a].transport != out[b].transport {
			return out[a].transport < out[b].transport
		}
		return out[a].mode == report.ModeConnPerRequest
	})
	return out
}

// addBars draws the groups side by side, one bar per server per workload, with
// each bar's value written along it.
//
// Every bar gets an X slot of its own rather than an offset from its group's
// tick: gonum offsets bars by a length on the page, while a label can only be
// placed at a coordinate on the axis, and mixing the two puts the labels
// somewhere near the bars instead of on them. With one slot per bar both live in
// axis units and cannot drift apart.
func (d *drawer) addBars(p *plot.Plot, groups []bars, order []string, label func(float64) string) error {
	const gap = 1 // Empty slots between one workload's group and the next.
	stride := len(groups) + gap
	slots := stride * len(order)

	tallest := 0.0
	labels := plotter.XYLabels{}
	for i, g := range groups {
		heights := make(plotter.Values, slots)
		for j, v := range g.values {
			slot := j*stride + i
			// A value at or below zero has no height to draw, and neither has a
			// workload this server was not measured in. Both are left to the
			// label, which says which of the two it was.
			if !math.IsNaN(v) && v > 0 {
				heights[slot] = v
				tallest = math.Max(tallest, v)
			}
			if math.IsNaN(v) {
				continue
			}
			text := label(v)
			if v <= 0 {
				// Subtracting what the measurement itself costs left nothing, or
				// less than nothing. The honest reading is zero within the error.
				text = "≈0"
			}
			labels.XYs = append(labels.XYs, plotter.XY{X: float64(slot), Y: math.Max(v, 0)})
			labels.Labels = append(labels.Labels, text)
		}
		chart, err := plotter.NewBarChart(heights, barWidth(slots))
		if err != nil {
			return err
		}
		chart.LineStyle.Width = 0
		chart.Color = d.barColor(g)
		p.Add(chart)
		p.Legend.Add(g.name(), chart)
	}
	if tallest == 0 {
		return errNoRows
	}

	valueLabels, err := plotter.NewLabels(labels)
	if err != nil {
		return err
	}
	for i := range valueLabels.TextStyle {
		// Turned on their side: eighteen horizontal labels across ten inches
		// would overlap, and a label that cannot be read is worse than none.
		valueLabels.TextStyle[i].Font.Size = fontsize * 0.6
		valueLabels.TextStyle[i].Rotation = math.Pi / 2
		valueLabels.TextStyle[i].XAlign = draw.XCenter
		valueLabels.TextStyle[i].YAlign = draw.YBottom
	}
	p.Add(valueLabels)

	// One tick per workload, in the middle of its group of bars.
	ticks := make([]plot.Tick, len(order))
	for j, name := range order {
		ticks[j] = plot.Tick{Value: float64(j*stride) + float64(len(groups)-1)/2, Label: name}
	}
	p.X.Tick.Marker = plot.ConstantTicks(ticks)
	p.X.Min = -1
	p.X.Max = float64(slots) - float64(gap)
	p.Y.Tick.Marker = byteTicksLinear{}
	p.Y.Min = 0
	p.Y.Max = tallest * 1.4 // Headroom for the labels standing on the bars.
	return nil
}

// barWidth shares the plotting area out among the slots, leaving a hair between
// neighbouring bars.
func barWidth(slots int) vg.Length {
	return 0.85 * (plotWidth - 1.5*vg.Inch) / vg.Length(slots)
}

// barColor keeps a server the colour it has in every other figure, and tells its
// two connection modes apart by washing the keep-alive one out. A reader who
// learned the legend once should not have to learn it again.
func (d *drawer) barColor(g bars) color.Color {
	c := d.color(g.impl, g.transport)
	if g.mode != report.ModeKeepAlive {
		return c
	}
	r, gr, b, a := c.RGBA()
	blend := func(v uint32) uint8 { return uint8((v + 0xffff) / 2 >> 8) }
	return color.RGBA{R: blend(r), G: blend(gr), B: blend(b), A: uint8(a >> 8)}
}
