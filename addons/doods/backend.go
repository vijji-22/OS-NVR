// Copyright 2020-2021 The OS-NVR Authors.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation; version 2.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package doods

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"nvr"
	"nvr/addons/doods/odrpc"
	"nvr/pkg/ffmpeg"
	"nvr/pkg/log"
	"nvr/pkg/monitor"
	"nvr/pkg/storage"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
)

func init() {
	nvr.RegisterMonitorMainProcessHook(main)
	nvr.RegisterMonitorSubProcessHook(sub)
	nvr.RegisterLogSource([]string{"doods"})
}

func main(ctx context.Context, m *monitor.Monitor, args *string) {
	if m.Config["doodsEnable"] != "true" || useSubStream(m) {
		return
	}

	*args += genArgs(m)

	if err := start(ctx, m, false); err != nil {
		m.Log.Error().
			Src("doods").
			Monitor(m.ID()).
			Msgf("could not start: %v", err)
	}
}

func sub(ctx context.Context, m *monitor.Monitor, args *string) {
	if m.Config["doodsEnable"] != "true" || !useSubStream(m) {
		return
	}

	*args += genArgs(m)

	if err := start(ctx, m, true); err != nil {
		m.Log.Error().
			Src("doods").
			Monitor(m.ID()).
			Msgf("could not start: %v", err)
	}
}

func useSubStream(m *monitor.Monitor) bool {
	if m.SubInputEnabled() && m.Config["doodsUseSubStream"] == "true" {
		return true
	}
	return false
}

func genArgs(m *monitor.Monitor) string {
	pipePath := m.Env.SHMDir + "/doods/" + m.ID() + "/main.fifo"

	return " -c:v copy -map 0:v -f fifo -fifo_format mpegts" +
		" -drop_pkts_on_overflow 1 -attempt_recovery 1" +
		" -restart_with_keyframe 1 -recovery_wait_time 1 " + pipePath
}

func start(ctx context.Context, m *monitor.Monitor, useSubStream bool) error {
	detector, err := detectorByName(m.Config["doodsDetectorName"])
	if err != nil {
		return fmt.Errorf("could not get detectory: %v", err)
	}

	config, err := parseConfig(m, doodsIP)
	if err != nil {
		return fmt.Errorf("could not parse config: %v", err)
	}

	a := newAddon(m, config)

	if err := a.prepareEnvironment(); err != nil {
		return fmt.Errorf("could not prepare environment: %v", err)
	}

	var size string
	if useSubStream {
		size = m.Config["sizeSub"]
	} else {
		size = m.Config["sizeMain"]
	}

	outputWidth := int(detector.GetWidth())
	outputHeight := int(detector.GetHeight())

	inputs, err := parseInputs(size, m.Config["doodsCrop"], outputWidth, outputHeight)
	if err != nil {
		return fmt.Errorf("could not parse inputs: %v", err)
	}

	outputs, err := calculateOutputs(inputs)
	if err != nil {
		return fmt.Errorf("could not calculate ffmpeg outputs: %v", err)
	}
	a.outputs = outputs

	ffmpegArgs := a.generateFFmpegArgs(m.Config, outputs)

	a.wg.Add(1)
	go a.newFFmpeg(ffmpegArgs).start(ctx)

	return nil
}

type thresholds map[string]float64

type doodsConfig struct {
	ip              string
	duration        time.Duration
	recDuration     time.Duration
	thresholds      thresholds
	timestampOffset time.Duration
	detectorName    string
}

func parseConfig(m *monitor.Monitor, ip string) (*doodsConfig, error) {
	var t thresholds
	if err := json.Unmarshal([]byte(m.Config["doodsThresholds"]), &t); err != nil {
		return nil, fmt.Errorf("could not unmarshal thresholds: %v", err)
	}
	for key, thresh := range t {
		if thresh == -1 {
			delete(t, key)
		}
	}

	feedRate := m.Config["doodsFeedRate"]
	duration, err := ffmpeg.FeedRateToDuration(feedRate)
	if err != nil {
		return nil, err
	}

	recDurationFloat, err := strconv.ParseFloat(m.Config["doodsDuration"], 64)
	if err != nil {
		return nil, fmt.Errorf("could not parse doodsDuration: %v", err)
	}
	recDuration := time.Duration(recDurationFloat * float64(time.Second))

	timestampOffset, err := strconv.Atoi(m.Config["timestampOffset"])
	if err != nil {
		return nil, fmt.Errorf("could not parse timestamp offset %v", err)
	}

	return &doodsConfig{
		ip:              ip,
		duration:        duration,
		recDuration:     recDuration,
		thresholds:      t,
		timestampOffset: time.Duration(timestampOffset) * time.Millisecond,
		detectorName:    m.Config["doodsDetectorName"],
	}, nil
}

func newAddon(m *monitor.Monitor, c *doodsConfig) *addon {
	return &addon{
		c:       c,
		wg:      m.WG,
		id:      m.ID(),
		name:    m.Name(),
		log:     m.Log,
		trigger: m.Trigger,

		env: m.Env,

		runFFmpeg: runFFmpeg,
	}
}

type addon struct {
	c       *doodsConfig
	id      string
	wg      *sync.WaitGroup
	name    string
	log     *log.Logger
	trigger monitor.Trigger

	outputs *outputs

	env *storage.ConfigEnv

	runFFmpeg runFFmpegFunc
}

func (a *addon) fifoDir() string {
	return a.env.SHMDir + "/doods/" + a.id
}

func (a *addon) mainPipe() string {
	return a.fifoDir() + "/main.fifo"
}

func (a *addon) prepareEnvironment() error {
	if err := os.MkdirAll(a.fifoDir(), 0700); err != nil && err != os.ErrExist {
		return fmt.Errorf("could not make directory for pipe: %v", err)
	}
	if err := ffmpeg.MakePipe(a.mainPipe()); err != nil {
		return fmt.Errorf("could not make main pipe: %v", err)
	}

	return nil
}

type inputs struct {
	inputWidth   float64
	inputHeight  float64
	cropX        float64
	cropY        float64
	cropSize     float64
	outputWidth  float64
	outputHeight float64
}

func parseInputs(size string, rawCrop string, outputWidth int, outputHeight int) (*inputs, error) {
	split := strings.Split(size, "x")
	inputWidth, err := strconv.ParseFloat(split[0], 64)
	if err != nil {
		return nil, fmt.Errorf("could not parse input width: %v %v", err, split)
	}
	inputHeight, err := strconv.ParseFloat(split[1], 64)
	if err != nil {
		return nil, fmt.Errorf("could not parse input height: %v %v", err, split)
	}

	var crop [3]float64
	if err := json.Unmarshal([]byte(rawCrop), &crop); err != nil {
		return nil, fmt.Errorf("could not Unmarshal crop values: %v", err)
	}

	return &inputs{
		inputWidth:   inputWidth,
		inputHeight:  inputHeight,
		cropX:        crop[0],
		cropY:        crop[1],
		cropSize:     crop[2],
		outputWidth:  float64(outputWidth),
		outputHeight: float64(outputHeight),
	}, nil
}

type outputs struct {
	paddedWidth  string
	paddedHeight string
	scaledWidth  string
	scaledHeight string
	cropX        string
	cropY        string
	outputWidth  int
	outputHeight int

	paddingYmultiplier float32
	paddingXmultiplier float32
	uncropXfunc        func(float32) float32
	uncropYfunc        func(float32) float32
}

func calculateOutputs(i *inputs) (*outputs, error) { //nolint:funlen
	if i.inputWidth < i.outputWidth {
		return nil, fmt.Errorf("input width is less than output width, %v/%v", i.inputWidth, i.outputWidth)
	}
	if i.inputHeight < i.outputHeight {
		return nil, fmt.Errorf("input height is less than output height, %v/%v", i.inputHeight, i.outputHeight)
	}

	paddedWidth := i.outputWidth * 100 / i.cropSize
	paddedHeight := i.outputHeight * 100 / i.cropSize

	cropOutX := paddedWidth * i.cropX / 100
	cropOutY := paddedHeight * i.cropY / 100

	widthRatio := i.inputWidth / i.outputWidth
	heightRatio := i.inputHeight / i.outputHeight

	scaledWidth := paddedWidth
	scaledHeight := paddedHeight

	var paddingXmultiplier float64 = 1
	var paddingYmultiplier float64 = 1

	if widthRatio > heightRatio {
		scaledHeight = i.inputHeight * paddedWidth / i.inputWidth
		paddingYmultiplier = paddedHeight / scaledHeight
	} else if widthRatio < heightRatio {
		scaledWidth = i.inputWidth * paddedHeight / i.inputHeight
		paddingXmultiplier = paddedWidth / scaledWidth
	}

	if scaledWidth > i.inputWidth {
		return nil, fmt.Errorf("scaled width is greater than input width: %v/%v", scaledWidth, i.inputWidth)
	}

	uncropXfunc := func(input float32) float32 {
		newMin := paddedWidth * i.cropX / 100
		newMax := paddedWidth * (i.cropX + i.cropSize) / 100
		newRange := newMax - newMin
		return float32((float64(input)*newRange + newMin) / paddedWidth)
	}
	uncropYfunc := func(input float32) float32 {
		newMin := paddedHeight * i.cropY / 100
		newMax := (paddedHeight * (i.cropY + i.cropSize) / 100)
		newRange := newMax - newMin
		return float32((float64(input)*newRange + newMin) / paddedHeight)
	}

	return &outputs{
		paddedWidth:  strconv.Itoa(int(paddedWidth)),
		paddedHeight: strconv.Itoa(int(paddedHeight)),
		scaledWidth:  strconv.Itoa(int(scaledWidth)),
		scaledHeight: strconv.Itoa(int(scaledHeight)),
		cropX:        strconv.Itoa(int(cropOutX)),
		cropY:        strconv.Itoa(int(cropOutY)),
		outputWidth:  int(i.outputWidth),
		outputHeight: int(i.outputHeight),

		paddingYmultiplier: float32(paddingYmultiplier),
		paddingXmultiplier: float32(paddingXmultiplier),
		uncropXfunc:        uncropXfunc,
		uncropYfunc:        uncropYfunc,
	}, nil
}

func (a *addon) generateFFmpegArgs(config monitor.Config, i *outputs) []string {
	// Output
	// ffmpeg -hwaccel x -i main.pipe -filter
	//   'fps=fps=3,scale=320:260,pad=320:320:0:0,crop:300:300:10:10'
	//   -f rawvideo -pix_fmt rgb24 -
	// Padding is done after scaling for higher efficiency.
	// Cropping must come after padding.

	logLevel := config["logLevel"]
	hwaccel := config["hwaccel"]
	fps := config["doodsFeedRate"]
	outputWidth := strconv.Itoa(i.outputWidth)
	outputHeight := strconv.Itoa(i.outputHeight)

	var args []string

	args = append(args, "-y", "-loglevel", logLevel)

	if hwaccel != "" {
		args = append(args, ffmpeg.ParseArgs("-hwaccel "+hwaccel)...)
	}

	args = append(args, "-i", a.mainPipe(), "-filter")
	args = append(args, "fps=fps="+fps+
		",scale="+i.scaledWidth+":"+i.scaledHeight+
		",pad="+i.paddedWidth+":"+i.paddedHeight+":0:0"+
		",crop="+outputWidth+":"+outputHeight+":"+i.cropX+":"+i.cropY)

	args = append(args, "-f", "rawvideo")
	args = append(args, "-pix_fmt", "rgb24", "-")

	return args
}

func (a *addon) newFFmpeg(args []string) *ffmpegConfig {
	return &ffmpegConfig{
		a:    a,
		args: args,

		d: &doodsClient{
			a:         a,
			c:         a.c,
			runClient: runClient,
			encoder: png.Encoder{
				CompressionLevel: png.BestSpeed,
			},
			sendFrame: sendFrame,
		},

		newProcess:  ffmpeg.NewProcess,
		runFFmpeg:   runFFmpeg,
		startClient: startClient,
	}
}

type ffmpegConfig struct {
	a    *addon
	d    *doodsClient
	args []string

	runFFmpeg   runFFmpegFunc
	newProcess  newProcessFunc
	startClient startClientFunc
}

type runFFmpegFunc func(context.Context, *ffmpegConfig) error
type newProcessFunc func(*exec.Cmd) ffmpeg.Process

func (f *ffmpegConfig) start(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			f.a.wg.Done()
			f.a.log.Info().Src("doods").Monitor(f.a.id).Msg("process stopped")
			return
		}
		if err := f.runFFmpeg(ctx, f); err != nil {
			f.a.log.Error().
				Src("doods").
				Monitor(f.a.id).
				Msgf("process crashed: %v", err)

			time.Sleep(1 * time.Second)
		}
	}
}

func runFFmpeg(ctx context.Context, f *ffmpegConfig) error {
	cmd := exec.Command(f.a.env.FFmpegBin, f.args...)
	process := f.newProcess(cmd)
	process.SetPrefix(f.a.name + ": doods: process: ")
	process.SetStderrLogger(f.a.log)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stderr: %v", err)
	}

	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()

	f.d.stdout = stdout
	f.a.wg.Add(1)
	go f.startClient(ctx2, f.d)

	f.a.log.Info().
		Src("doods").
		Monitor(f.a.id).
		Msgf("starting process: %v", cmd)

	if err = process.Start(ctx); err != nil {
		return fmt.Errorf("detector crashed: %v", err)
	}
	cancel()
	return nil
}

type doodsClient struct {
	a *addon
	c *doodsConfig

	stdout io.Reader
	stream *odrpc.OdrpcDetectStreamClient

	runClient runClientFunc
	encoder   png.Encoder
	sendFrame sendFrameFunc
}

type startClientFunc func(context.Context, *doodsClient)
type runClientFunc func(context.Context, *doodsClient) error
type sendFrameFunc func(*doodsClient, time.Time, *bytes.Buffer) error

func startClient(ctx context.Context, d *doodsClient) {
	for {
		if ctx.Err() != nil {
			d.a.log.Info().
				Src("doods").
				Monitor(d.a.id).
				Msg("client stopped")

			d.a.wg.Done()
			return
		}
		if err := d.runClient(ctx, d); err != nil {
			d.a.log.Error().
				Src("doods").
				Monitor(d.a.id).
				Msgf("client crashed: %v", err)

			time.Sleep(1 * time.Second)
		}
	}
}

var dialOptions = []grpc.DialOption{
	grpc.WithBlock(),
	grpc.WithInsecure(),
}

func runClient(ctx context.Context, d *doodsClient) error {
	ctx2, cancel := context.WithTimeout(ctx, 3000*time.Millisecond)
	defer cancel()

	conn, err := grpc.DialContext(ctx2, d.c.ip, dialOptions...)
	if err != nil {
		return fmt.Errorf("could not connect to server: %v", err)
	}
	defer conn.Close()

	rpcClient := odrpc.NewOdrpcClient(conn)

	d.stream, err = rpcClient.DetectStream(ctx)
	if err != nil {
		return fmt.Errorf("could not open stream: %v", err)
	}
	if err := d.readFrames(ctx); err != nil {
		return fmt.Errorf("could not read frames: %v", err)
	}

	if err := d.stream.CloseSend(); err != nil {
		return fmt.Errorf("could not close stream: %v", err)
	}

	return nil
}

func (d *doodsClient) readFrames(ctx context.Context) error {
	outputWidth := d.a.outputs.outputWidth
	outputHeight := d.a.outputs.outputHeight

	rect := image.Rect(0, 0, outputWidth, outputHeight)
	frameSize := outputWidth * outputHeight * 3

	tmp := make([]byte, frameSize)
	for {
		if ctx.Err() != nil {
			return nil
		}
		if _, err := io.ReadAtLeast(d.stdout, tmp, frameSize); err != nil {
			if err == io.EOF {
				fmt.Println("eof")
				return nil
			}
			return fmt.Errorf("could not read from stdout: %v", err)
		}
		t := time.Now()

		t.Add(-d.c.timestampOffset)

		img := NewRGB24(rect)
		img.Pix = tmp

		var b bytes.Buffer
		_ = d.encoder.Encode(&b, img)

		err := d.sendFrame(d, t, &b)
		if err != nil {
			return fmt.Errorf("could not send frame: %v", err)
		}
	}
}

func sendFrame(d *doodsClient, t time.Time, b *bytes.Buffer) error {
	request := &odrpc.DetectRequest{
		DetectorName: d.c.detectorName,
		Data:         b.Bytes(),
		Detect: map[string]float32{
			"*": 10,
		},
	}
	//fmt.Println("sending")
	if err := d.stream.Send(request); err != nil {
		return fmt.Errorf("could not send: %v", err)
	}

	response, err := d.stream.Recv()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not receive: %v", err)
	}

	d.a.parseDetections(t, response.Detections)
	return nil
}

func (a *addon) parseDetections(t time.Time, detections []*odrpc.Detection) {
	if len(detections) == 0 {
		return
	}

	filtered := []monitor.Detection{}

	for _, detection := range detections {
		score := float64(detection.GetConfidence())
		label := detection.GetLabel()

		for name, thresh := range a.c.thresholds {
			if label != name || score < thresh {
				continue
			}

			convX := func(input float32) int {
				return int(a.outputs.uncropXfunc(input) *
					a.outputs.paddingXmultiplier * 100)
			}
			convY := func(input float32) int {
				return int(a.outputs.uncropYfunc(input) *
					a.outputs.paddingYmultiplier * 100)
			}

			d := monitor.Detection{
				Label: label,
				Score: score,
				Region: &monitor.Region{
					Rect: &ffmpeg.Rect{
						convY(detection.GetTop()),
						convX(detection.GetLeft()),
						convY(detection.GetBottom()),
						convX(detection.GetRight()),
					},
				},
			}
			filtered = append(filtered, d)
		}
	}

	if len(filtered) != 0 {
		now := time.Now().Local()
		timestamp := fmt.Sprintf("%v:%v:%v", now.Hour(), now.Minute(), now.Second())

		a.log.Info().
			Src("doods").
			Monitor(a.id).
			Msgf("trigger: label:%v score:%.1f time:%v",
				filtered[0].Label, filtered[0].Score, timestamp)

		a.trigger <- monitor.Event{
			Time:        t,
			Detections:  filtered,
			Duration:    a.c.duration,
			RecDuration: a.c.recDuration,
		}
	}
}
