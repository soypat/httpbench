package main

import (
	"image/color"
	"math"
	"os"

	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
	"gonum.org/v1/plot/vg/draw"
	"gonum.org/v1/plot/vg/vgimg"

	"github.com/soypat/httpbench/internal/report"
)

// drawStaticFootprint is the figure one binary per implementation makes
// possible: a size, a resident set and a per-connection cost that each belong to
// one stack instead of to a process with every stack linked into it.
//
// The three quantities are megabytes, megabytes and kilobytes, so they get a
// panel each rather than a shared axis on which the smallest would be invisible.
// Every bar is labelled with its value, because a bar of no height is not a
// number a reader can see and zero is the whole point of one of these columns.
func (d *drawer) drawStaticFootprint(rep *report.Report, path string) error {
	var labels []string
	var colors []color.Color
	var binary, rss, perConn plotter.Values
	for _, r := range rep.Results {
		if r.Kind != report.KindStatic {
			continue
		}
		labels = append(labels, r.Impl+"\n"+report.ConnTypeName(r.Transport))
		colors = append(colors, d.color(r.Impl, r.Transport))
		binary = append(binary, float64(r.BinaryBytes))
		rss = append(rss, float64(r.RSSAtRest))
		// A server that closes every connection holds nothing per connection.
		// That is a zero on this figure, not a missing bar.
		perConn = append(perConn, idleHeapFor(rep, r.Impl, r.Transport))
	}
	if len(labels) == 0 {
		return errNoRows
	}

	panels := []struct {
		title  string
		values plotter.Values
	}{
		{"Stripped binary", binary},
		{"RSS at rest", rss},
		{"Held per idle connection", perConn},
	}
	plots := make([]*plot.Plot, len(panels))
	for i, panel := range panels {
		p, err := d.barPanel(panel.title, labels, colors, panel.values)
		if err != nil {
			return err
		}
		plots[i] = p
	}

	// One row of panels on a shared canvas: gonum aligns their plotting areas so
	// the implementation names line up under all three.
	img := vgimg.New(plotWidth, plotHeight)
	tiles := draw.Tiles{
		Rows: 1, Cols: len(plots),
		PadX:      vg.Millimeter * 3,
		PadTop:    vg.Millimeter * 2,
		PadBottom: vg.Millimeter * 2,
		PadLeft:   vg.Millimeter * 2,
		PadRight:  vg.Millimeter * 2,
	}
	canvases := plot.Align([][]*plot.Plot{plots}, tiles, draw.New(img))
	for i, p := range plots {
		p.Draw(canvases[0][i])
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	png := vgimg.PngCanvas{Canvas: img}
	if _, err = png.WriteTo(f); err != nil {
		return err
	}
	return f.Close()
}

// barPanel draws one quantity as a bar per server, with the value written above
// each bar. A bar is drawn as its own chart so that it can carry the colour its
// server has in every other figure; the rest of that chart's bars are zero and
// draw nothing.
func (d *drawer) barPanel(title string, labels []string, colors []color.Color, values plotter.Values) (*plot.Plot, error) {
	p := newPlot(title, "", "")
	p.Legend.Top, p.Legend.Left = false, false
	p.Title.TextStyle.Font.Size = fontsize
	p.X.Tick.Label.Font.Size = fontsize * 0.7

	width := vg.Points(24)
	for i := range values {
		one := make(plotter.Values, len(values))
		one[i] = values[i]
		bars, err := plotter.NewBarChart(one, width)
		if err != nil {
			return nil, err
		}
		bars.LineStyle.Width = 0
		bars.Color = colors[i]
		p.Add(bars)
	}

	tallest := 0.0
	xy := plotter.XYLabels{}
	for i, v := range values {
		tallest = math.Max(tallest, v)
		xy.XYs = append(xy.XYs, plotter.XY{X: float64(i), Y: v})
		xy.Labels = append(xy.Labels, decimalBytes(v))
	}
	valueLabels, err := plotter.NewLabels(xy)
	if err != nil {
		return nil, err
	}
	for i := range valueLabels.TextStyle {
		valueLabels.TextStyle[i].Font.Size = fontsize * 0.7
		valueLabels.TextStyle[i].XAlign = draw.XCenter
	}
	p.Add(valueLabels)

	p.NominalX(labels...)
	p.Y.Tick.Marker = byteTicksLinear{}
	p.Y.Min = 0
	p.Y.Max = tallest * 1.25 // Headroom for the labels.
	if tallest == 0 {
		p.Y.Max = 1
	}
	return p, nil
}

// idleHeapFor is what one open, idle connection cost that server, or zero if it
// held nothing — which is the answer for a server that closes every connection
// rather than a gap in the measurement.
func idleHeapFor(rep *report.Report, impl, transport string) float64 {
	for _, r := range rep.Results {
		if r.Kind == report.KindIdle && r.Impl == impl && r.Transport == transport {
			return math.Max(0, r.HeapPerConn+r.StackPerConn)
		}
	}
	return 0
}
