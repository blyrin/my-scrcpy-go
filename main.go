package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

var mainWin *mygo.Window

func main() {
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}
	mygo.App.WhenReady(func() {
		st := newDataStore()
		app := newApp(st, nil)
		win := mygo.NewWindow(mygo.WindowOptions{
			Title:     "my scrcpy go",
			Width:     1000,
			Height:    780,
			MinWidth:  680,
			MinHeight: 480,
			StateKey:  "main",
			Content:   ui.View(app.View),
		})
		app.win = win
		mainWin = win
		app.Init()
	})
	mygo.App.OnSecondInstance(func(args []string, workingDir string) {
		if mainWin != nil {
			mainWin.Focus()
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

const (
	modeMirror  = "mirror"
	modeCamera  = "camera"
	modeDesktop = "desktop"
)

var audioFallbackChain = [...]string{"aac", "flac", "raw"}

type config struct {
	Device                        string  `json:"device"`
	SessionMode                   string  `json:"sessionMode"`
	Bitrate                       float64 `json:"bitrate"`
	FPS                           string  `json:"fps"`
	StayAwake                     bool    `json:"stayAwake"`
	TurnOff                       bool    `json:"turnOff"`
	KeepActive                    bool    `json:"keepActive"`
	AudioEnabled                  bool    `json:"audioEnabled"`
	AudioCodec                    string  `json:"audioCodec"`
	AlwaysOnTop                   bool    `json:"alwaysOnTop"`
	Fullscreen                    bool    `json:"fullscreen"`
	Borderless                    bool    `json:"borderless"`
	Record                        bool    `json:"record"`
	RecordPath                    string  `json:"recordPath"`
	ScrcpyPath                    string  `json:"scrcpyPath"`
	OtgPure                       bool    `json:"otgPure"`
	CameraFacing                  string  `json:"cameraFacing"`
	CameraID                      string  `json:"cameraId"`
	Codec                         string  `json:"codec"`
	CameraAr                      string  `json:"cameraAr"`
	CameraHighSpeed               bool    `json:"cameraHighSpeed"`
	VdWidth                       float64 `json:"vdWidth"`
	VdHeight                      float64 `json:"vdHeight"`
	VdDpi                         float64 `json:"vdDpi"`
	AspectLock                    bool    `json:"aspectLock"`
	Rotation                      string  `json:"rotation"`
	Res                           string  `json:"res"`
	HidKeyboard                   bool    `json:"hidKeyboard"`
	HidMouse                      bool    `json:"hidMouse"`
	RenderDriver                  string  `json:"renderDriver"`
	FlexDisplay                   bool    `json:"flexDisplay"`
	CameraTorch                   bool    `json:"cameraTorch"`
	CameraZoom                    float64 `json:"cameraZoom"`
	BackgroundColor               string  `json:"backgroundColor"`
	Vsync                         bool    `json:"vsync"`
	IgnoreVideoEncoderConstraints bool    `json:"ignoreVideoEncoderConstraints"`
}

func (c *config) shouldAutoFallback() bool {
	codec := strings.TrimSpace(c.AudioCodec)
	return c.AudioEnabled && (codec == "" || strings.EqualFold(codec, "auto"))
}

type mdnsService struct {
	Name    string
	Service string
	Address string
}

type camera struct {
	ID   string
	Name string
}

type deviceList struct {
	Err     string
	Devices []string
	Models  map[string]string
}

type adbResult struct {
	OK      bool
	Message string
}

type terminalResult struct {
	OK     bool
	Binary string
	Stdout string
	Stderr string
	Err    string
}

type driverOption struct {
	ID    string
	Label string
}

type runner struct {
	mu      sync.Mutex
	procs   map[string]*exec.Cmd
	log     func(string)
	running func(device string, running bool)
}

type alertState struct {
	open     bool
	title    string
	message  string
	action   string
	onAction func()
}

type pairState struct {
	open    bool
	target  mdnsService
	waiting bool
	code    string
	err     string
	polling bool
}

type updateState struct {
	checkedPath string
	local       string
	latest      string
}

type app struct {
	cfg config

	devices       []string
	deviceModels  map[string]string
	friendlyNames map[string]string
	fetchedNames  map[string]bool
	activeDevice  string
	deviceSel     int
	deviceList    ui.ListState
	consolePct    float32
	consoleList   ui.ListState
	runningSet    map[string]bool

	logs []string

	isRefreshing  bool
	isDownloading bool
	downloadPct   float64

	scrcpyFound   bool
	scrcpyMessage string

	detectedCameras []camera
	renderSupported bool
	renderDrivers   []driverOption

	mdnsDevices []mdnsService
	history     []string

	onboardingDone bool
	onboardingOpen bool
	guideOpen      bool

	pair   pairState
	alert  alertState
	update updateState

	defaultRecordPath string

	sidebarPercent float32
	shortcutsOpen  bool
	connectIP      string
	manualPairIP   string
	manualPairCode string
	commandInput   string

	prevVdW float64
	prevVdH float64

	r     *runner
	store *dataStore
	win   *mygo.Window
}

func (a *app) onMain(fn func()) {
	if a.win != nil {
		a.win.Update(fn)
		return
	}
	fn()
}

func (a *app) View(c *ui.Context) { a.view(c) }

func (a *app) appendLog(lines ...string) {
	stamp := time.Now().Format("15:04:05")
	for _, l := range lines {
		a.logs = append(a.logs, "["+stamp+"] "+l)
	}
	if len(a.logs) > 100 {
		a.logs = a.logs[len(a.logs)-100:]
	}
}

func (a *app) clearLogs() { a.logs = nil }

func (a *app) saveConfig() {
	if a.store != nil {
		_ = a.store.save("config.json", &a.cfg)
	}
}

func (a *app) saveHistory() {
	if a.store != nil {
		_ = a.store.save("history.json", a.history)
	}
}

func (a *app) addToHistory(ip string) {
	if !strings.Contains(ip, ":") {
		return
	}
	a.history = slices.DeleteFunc(a.history, func(s string) bool { return s == ip })
	a.history = slices.Insert(a.history, 0, ip)
	if len(a.history) > 10 {
		a.history = a.history[:10]
	}
	a.saveHistory()
}

func (a *app) sessionRunning() bool {
	return a.activeDevice != "" && a.runningSet[a.activeDevice]
}

func (a *app) showAlert(title, message, action string, onAction func()) {
	a.alert.open = true
	a.alert.title = title
	a.alert.message = message
	a.alert.action = action
	a.alert.onAction = onAction
}

func (a *app) closeAlert() {
	a.alert.open = false
	a.alert.onAction = nil
}

func (a *app) view(c *ui.Context) {
	t := c.Theme()
	root := ui.Column(c).Fill().Background(t.Background).Children(func() {
		ui.SplitVerticalPercent(c, &a.consolePct,
			func() {
				ui.SplitPercent(c, &a.sidebarPercent,
					func() { a.drawSidebar(c) },
					func() { a.drawMain(c) },
				).Fill()
			},
			func() { a.drawConsole(c) },
		).Fill().Grow(1)
		a.drawOnboarding(c)
		a.drawPairModal(c)
		a.drawGuideModal(c)
		a.drawAlertDialog(c)
		a.drawShortcutsModal(c)
	})
	if files := root.DroppedFiles(); len(files) > 0 {
		a.pushOrInstall(files)
	}
	a.prevVdW = a.cfg.VdWidth
	a.prevVdH = a.cfg.VdHeight
}

func formatPct(p float64) string {
	return strconv.Itoa(min(max(int(p), 0), 100)) + "%"
}

func (a *app) drawSidebar(c *ui.Context) {
	t := c.Theme()
	ui.Scroll(c).Fill().Children(func() {
		ui.Column(c).FillWidth().Gap(16).Padding(12).Children(func() {
			ui.Fieldset(c, "核心", func() {
				status := a.scrcpyMessage
				color := t.TextMuted
				switch {
				case a.isDownloading:
					status = "下载中 " + formatPct(a.downloadPct)
					color = t.Accent
				case a.scrcpyFound:
					color = t.Success
				case a.scrcpyMessage != "加载中...":
					color = t.Danger
				}
				ui.Text(c, status).TextColor(color)
				if a.isDownloading {
					ui.Progress(c, a.downloadPct/100).FillWidth()
				}
				ui.Row(c).FillWidth().Wrap().Gap(6).Children(func() {
					if !a.scrcpyFound && !a.isDownloading {
						ui.PrimaryButton(c, "安装核心").OnClick(a.downloadScrcpy)
						ui.Button(c, "安装指南").OnClick(func() { a.guideOpen = true })
					}
					ui.Button(c, "选择文件夹").Tooltip("手动指定 scrcpy 所在目录").OnClick(a.chooseScrcpyFolder)
					if a.cfg.ScrcpyPath != "" {
						ui.Button(c, "重置路径").OnClick(a.resetScrcpyPath)
					}
					ui.Button(c, "快捷键").OnClick(func() { a.shortcutsOpen = true })
				})
			})
			ui.Fieldset(c, "设备", func() {
				refreshBtn := "刷新"
				if a.isRefreshing {
					refreshBtn = "同步中..."
				}
				ui.Row(c).FillWidth().Gap(8).Children(func() {
					ui.Button(c, refreshBtn).Disabled(a.isRefreshing).OnClick(a.refreshDevices)
					ui.Spacer(c).Grow(1)
					ui.Button(c, "终止 ADB").OnClick(a.killAdb)
				})
				if len(a.devices) == 0 {
					ui.Text(c, "未发现设备，点击刷新").TextColor(t.TextMuted)
					return
				}
				a.deviceList.Selected = &a.deviceSel
				a.deviceList.Label = func(i int) string { return a.deviceLabel(a.devices[i]) }
				ui.List(c, &a.deviceList, len(a.devices)).
					FillWidth().MaxHeight(280).
					ItemKey(func(i int) any { return a.devices[i] }).
					Rows(func(r ui.ListRow) { a.deviceRow(r, a.devices[r.Index]) }).
					OnChange(func() {
						if a.deviceSel >= 0 && a.deviceSel < len(a.devices) {
							a.activeDevice = a.devices[a.deviceSel]
						}
					})
			})
			ui.Button(c, "发送文件到设备").FillWidth().
				Tooltip("APK 自动安装，其他文件推送到 /sdcard/Download；也可以直接拖入窗口").
				OnClick(a.browseFiles)
			ui.Fieldset(c, "无线连接", func() {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.End).Children(func() {
					ui.TextInput(c, &a.connectIP).Placeholder("ip:port").Grow(1)
					ui.Button(c, "连接").OnClick(func() { a.connectDevice(a.connectIP) })
				})
				a.drawMdnsSection(c)
				a.drawHistorySection(c)
				a.drawManualPair(c)
			})
		})
	})
}

func (a *app) drawMain(c *ui.Context) {
	ui.Column(c).Fill().Padding(12).Gap(16).Children(func() {
		a.drawControlPanel(c)
		a.drawSessionBehavior(c)
	})
}

func (a *app) drawConsole(c *ui.Context) {
	a.consoleList.FollowEnd = true
	ui.Column(c).Fill().Children(func() {
		ui.Row(c).FillWidth().Padding(6, 10).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "系统控制台").TextColor(c.Theme().TextMuted)
			ui.Spacer(c).Grow(1)
			ui.Button(c, "导出报告").OnClick(a.exportReport)
			ui.Button(c, "清空").OnClick(a.clearLogs)
		})
		if len(a.logs) > 0 {
			ui.List(c, &a.consoleList, len(a.logs)).
				Fill().Grow(1).Padding(0, 10).
				Rows(func(r ui.ListRow) {
					ui.Text(r.Context, a.logs[r.Index]).
						Font("monospace").FontSize(11).
						TextColor(a.logColor(r.Context, a.logs[r.Index])).
						SingleLine().Ellipsis("…")
				})
		} else {
			ui.Spacer(c).Grow(1)
		}
		ui.TextInput(c, &a.commandInput).Placeholder("输入 adb / scrcpy 命令，回车执行").
			FillWidth().Margin(6, 10, 10, 10).OnSubmit(func() {
			a.runCommand(a.commandInput)
			a.commandInput = ""
		})
	})
}

func (a *app) logColor(c *ui.Context, line string) ui.Color {
	t := c.Theme()
	mark := line
	if i := strings.Index(mark, "] "); i >= 0 && i < 10 {
		mark = mark[i+2:]
	}
	switch {
	case strings.HasPrefix(mark, "[SYSTEM"):
		return t.TextMuted
	case strings.Contains(mark, "ERROR"), strings.Contains(mark, "错误"), strings.Contains(mark, "失败"):
		return t.Danger
	}
	return t.Text
}

func (a *app) deviceRow(r ui.ListRow, serial string) {
	c := r.Context
	t := c.Theme()
	kind := "USB"
	if strings.Contains(serial, ".") {
		kind = "WiFi"
	}
	running := a.runningSet[serial]
	ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Padding(6, 8).Children(func() {
		ui.Column(c).Gap(2).Grow(1).Children(func() {
			ui.Text(c, a.deviceLabel(serial)).FontSize(12).Bold().SingleLine().Ellipsis("…")
			ui.Text(c, formatSerial(serial)).FontSize(10).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
		})
		ui.Badge(c, kind)
		if running {
			ui.Badge(c, "LIVE")
		}
	})
}

func formatSerial(serial string) string {
	return strings.TrimSuffix(strings.TrimPrefix(serial, "adb-"), "._adb-tls-connect._tcp")
}

func (a *app) drawMdnsSection(c *ui.Context) {
	t := c.Theme()
	var pending []mdnsService
	for _, s := range a.mdnsDevices {
		if !a.mdnsConnected(s) {
			pending = append(pending, s)
		}
	}
	if len(pending) == 0 {
		return
	}
	ui.Divider(c)
	ui.Text(c, "附近设备").TextColor(t.TextMuted)
	for _, s := range pending {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Padding(4, 0).Children(func() {
			ui.Column(c).Gap(2).Grow(1).Children(func() {
				ui.Text(c, s.Name).SingleLine().Ellipsis("…")
				ui.Text(c, s.Address).FontSize(11).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
			})
			ui.Button(c, "配对").OnClick(func() { a.openPairModal(s) })
		})
	}
}

func (a *app) mdnsConnected(s mdnsService) bool {
	return slices.ContainsFunc(a.devices, func(d string) bool {
		return d == s.Address || strings.Contains(d, s.Name)
	})
}

func (a *app) drawHistorySection(c *ui.Context) {
	t := c.Theme()
	if len(a.history) == 0 {
		return
	}
	ui.Divider(c)
	ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
		ui.Text(c, "最近连接").TextColor(t.TextMuted).Grow(1)
		ui.Button(c, "清空").OnClick(func() {
			a.history = nil
			a.saveHistory()
		})
	})
	for _, ip := range a.history {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Padding(4, 0).OnClick(func() {
			a.connectDevice(ip)
		}).Children(func() {
			ui.Text(c, ip).Grow(1).SingleLine().Ellipsis("…")
		})
	}
}

func (a *app) drawManualPair(c *ui.Context) {
	ui.Divider(c)
	ui.Row(c).FillWidth().Gap(8).Children(func() {
		ui.TextInput(c, &a.manualPairIP).Placeholder("配对 ip:port").Grow(1)
		ui.TextInput(c, &a.manualPairCode).Placeholder("配对码").Width(90)
	})
	ui.PrimaryButton(c, "开始配对").FillWidth().OnClick(func() {
		if a.manualPairIP == "" || a.manualPairCode == "" {
			return
		}
		a.pairWithCode(a.manualPairIP, a.manualPairCode)
	})
}

var (
	resOptions      = []string{"原始", "4K", "2K", "1080p", "900p", "720p", "576p", "480p"}
	resValues       = map[string]string{"原始": "0", "4K": "3840", "2K": "2560", "1080p": "1920", "900p": "1600", "720p": "1280", "576p": "1024", "480p": "800"}
	codecOptions    = []string{"H.264", "H.265", "AV1", "VP8", "VP9"}
	codecValues     = map[string]string{"H.264": "h264", "H.265": "h265", "AV1": "av1", "VP8": "vp8", "VP9": "vp9"}
	fpsOptions      = []string{"自动", "30", "60", "90", "120"}
	rotationOptions = []string{"0°", "-90°", "+90°", "180°"}
	rotationValues  = map[string]string{"0°": "0", "-90°": "270", "+90°": "90", "180°": "180"}
	arOptions       = []string{"默认", "16:9", "4:3"}
	audioCodecs     = []string{"自动", "opus", "aac", "flac", "raw"}
)

func keyForValue(m map[string]string, value, fallback string) string {
	for k, v := range m {
		if v == value {
			return k
		}
	}
	return fallback
}

func (a *app) drawControlPanel(c *ui.Context) {
	ui.Fieldset(c, "采集源", func() {
		modeIdx := 0
		switch a.cfg.SessionMode {
		case modeCamera:
			modeIdx = 1
		case modeDesktop:
			modeIdx = 2
		}
		ui.Row(c).FillWidth().Children(func() {
			ui.Segmented(c, &modeIdx, "屏幕镜像", "相机", "桌面").Grow(1).OnChange(func() {
				a.cfg.SessionMode = [3]string{modeMirror, modeCamera, modeDesktop}[modeIdx]
				a.saveConfig()
			})
		})
		ui.Scroll(c).Fill().Grow(1).Children(func() {
			ui.Column(c).FillWidth().Gap(12).Children(func() {
				switch a.cfg.SessionMode {
				case modeCamera:
					a.cameraConfig(c)
				case modeDesktop:
					a.desktopConfig(c)
				default:
					a.mirrorConfig(c)
				}
			})
		})
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			a.statusBadge(c)
			a.startStopButton(c)
		})
	}).Fill().Grow(1)
}

func (a *app) statusBadge(c *ui.Context) {
	t := c.Theme()
	text := "就绪"
	if a.sessionRunning() {
		text = "运行中"
	}
	ui.Box(c).Padding(0, t.Space(1.5)).Radius(999).Background(t.Text.Alpha(0.1)).Shrink(0).
		Children(func() {
			ui.Text(c, text).FontSize(14).SingleLine()
		})
}

func (a *app) selectField(c *ui.Context, label string, sel *string, options []string, apply func()) {
	ui.Field(c, label, func() {
		ui.Select(c, sel, options).OnChange(apply)
	}).Grow(1).MinWidth(150)
}

func (a *app) perfGrid(c *ui.Context, showResolution, showCodec bool) {
	resSel := keyForValue(resValues, a.cfg.Res, "原始")
	codecSel := keyForValue(codecValues, a.cfg.Codec, "H.264")
	fpsSel := a.cfg.FPS
	if fpsSel == "" {
		fpsSel = "自动"
	}
	rotSel := keyForValue(rotationValues, a.cfg.Rotation, "0°")
	driverSel := a.rendererLabel()
	ui.Row(c).FillWidth().Wrap().Gap(8).AlignItems(ui.End).Children(func() {
		if showResolution {
			a.selectField(c, "分辨率", &resSel, resOptions, func() {
				a.cfg.Res = resValues[resSel]
				a.saveConfig()
			})
		}
		if showCodec {
			a.selectField(c, "编解码器", &codecSel, codecOptions, func() {
				a.cfg.Codec = codecValues[codecSel]
				a.saveConfig()
			})
		}
		a.selectField(c, "帧率", &fpsSel, fpsOptions, func() {
			if fpsSel == "自动" {
				a.cfg.FPS = ""
			} else {
				a.cfg.FPS = fpsSel
			}
			a.saveConfig()
		})
		a.selectField(c, "旋转", &rotSel, rotationOptions, func() {
			a.cfg.Rotation = rotationValues[rotSel]
			a.saveConfig()
		})
		a.selectField(c, "渲染器", &driverSel, a.rendererOptions(), func() {
			a.cfg.RenderDriver = a.rendererID(driverSel)
			a.saveConfig()
		})
	})
	a.bitrateSlider(c)
	ui.Row(c).FillWidth().Wrap().Gap(16).Children(func() {
		ui.Checkbox(c, &a.cfg.Vsync, "垂直同步").Tooltip("开启 SDL 渲染器 VSync，防止画面撕裂").OnChange(a.saveConfig)
		ui.Checkbox(c, &a.cfg.IgnoreVideoEncoderConstraints, "忽略编码器约束").Tooltip("设备上报错误的编码器尺寸限制时使用（scrcpy v4.1+）").OnChange(a.saveConfig)
	})
}

func (a *app) rendererOptions() []string {
	opts := []string{"自动"}
	for _, d := range a.renderDrivers {
		opts = append(opts, d.Label)
	}
	return opts
}

func (a *app) rendererLabel() string {
	if a.cfg.RenderDriver == "" || a.cfg.RenderDriver == "auto" {
		return "自动"
	}
	for _, d := range a.renderDrivers {
		if d.ID == a.cfg.RenderDriver {
			return d.Label
		}
	}
	return "自动"
}

func (a *app) rendererID(label string) string {
	if label == "自动" {
		return ""
	}
	for _, d := range a.renderDrivers {
		if d.Label == label {
			return d.ID
		}
	}
	return ""
}

func (a *app) bitrateSlider(c *ui.Context) {
	ui.Field(c, "码率", func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Slider(c, &a.cfg.Bitrate, 1, 50).Grow(1).OnChange(a.saveConfig)
			ui.Text(c, strconv.Itoa(int(a.cfg.Bitrate))+"M")
		})
	})
}

func (a *app) mirrorConfig(c *ui.Context) {
	t := c.Theme()
	ui.Fieldset(c, "输入增强 (HID)", func() {
		ui.Row(c).FillWidth().Wrap().Gap(16).Children(func() {
			ui.Checkbox(c, &a.cfg.HidKeyboard, "HID 键盘").Tooltip("通过 UHID 转发键盘（scrcpy 2.4+，需要 Android 6+）").OnChange(a.saveConfig)
			ui.Checkbox(c, &a.cfg.HidMouse, "HID 鼠标").Tooltip("通过 UHID 转发鼠标").OnChange(a.saveConfig)
			if a.cfg.HidKeyboard || a.cfg.HidMouse {
				ui.Checkbox(c, &a.cfg.OtgPure, "纯 HID (OTG)").Tooltip("仅转发键鼠，不镜像画面").OnChange(a.saveConfig)
			}
		})
	})
	if !a.cfg.OtgPure {
		a.perfGrid(c, true, true)
		return
	}
	ui.Text(c, "纯 HID 模式已启用，画面与码率设置不可用").TextColor(t.TextMuted)
}

func (a *app) cameraConfig(c *ui.Context) {
	ui.Fieldset(c, "摄像头", func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.End).Children(func() {
			camSel := a.cameraLabel()
			ui.Select(c, &camSel, a.cameraOptions()).Grow(1).OnChange(func() {
				a.cfg.CameraID = ""
				if camSel != "自动选择" {
					for _, cam := range a.detectedCameras {
						if cam.Name == camSel {
							a.cfg.CameraID = cam.ID
						}
					}
				}
				a.saveConfig()
			})
			ui.Button(c, "刷新镜头").OnClick(a.listCameras)
		})
	})
	codecSel := keyForValue(codecValues, a.cfg.Codec, "H.264")
	fpsSel := a.cfg.FPS
	if fpsSel == "" {
		fpsSel = "自动"
	}
	resSel := keyForValue(resValues, a.cfg.Res, "原始")
	arSel := "默认"
	if a.cfg.CameraAr == "16:9" || a.cfg.CameraAr == "4:3" {
		arSel = a.cfg.CameraAr
	}
	ui.Row(c).FillWidth().Wrap().Gap(8).AlignItems(ui.End).Children(func() {
		a.selectField(c, "编解码器", &codecSel, codecOptions, func() {
			a.cfg.Codec = codecValues[codecSel]
			a.saveConfig()
		})
		a.selectField(c, "宽高比", &arSel, arOptions, func() {
			if arSel == "16:9" || arSel == "4:3" {
				a.cfg.CameraAr = arSel
			} else {
				a.cfg.CameraAr = "0"
			}
			a.saveConfig()
		})
		a.selectField(c, "帧率", &fpsSel, fpsOptions, func() {
			if fpsSel == "自动" {
				a.cfg.FPS = ""
			} else {
				a.cfg.FPS = fpsSel
			}
			a.saveConfig()
		})
		a.selectField(c, "分辨率", &resSel, resOptions, func() {
			a.cfg.Res = resValues[resSel]
			a.saveConfig()
		})
	})
	ui.Fieldset(c, "v4 相机控制", func() {
		ui.Checkbox(c, &a.cfg.CameraTorch, "手电筒").OnChange(a.saveConfig)
		ui.Field(c, "变焦", func() {
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.StepSlider(c, &a.cfg.CameraZoom, 1.0, 5.0, 0.1).Grow(1).OnChange(a.saveConfig)
				ui.Text(c, strconv.FormatFloat(a.cfg.CameraZoom, 'f', 1, 64)+"x")
			})
		})
	})
	a.bitrateSlider(c)
	ui.Row(c).FillWidth().Wrap().Gap(16).Children(func() {
		ui.Checkbox(c, &a.cfg.Vsync, "垂直同步").OnChange(a.saveConfig)
		ui.Checkbox(c, &a.cfg.IgnoreVideoEncoderConstraints, "忽略编码器约束").OnChange(a.saveConfig)
	})
}

func (a *app) cameraOptions() []string {
	opts := []string{"自动选择"}
	for _, cam := range a.detectedCameras {
		opts = append(opts, cam.Name)
	}
	return opts
}

func (a *app) cameraLabel() string {
	if a.cfg.CameraID == "" {
		return "自动选择"
	}
	for _, cam := range a.detectedCameras {
		if cam.ID == a.cfg.CameraID {
			return cam.Name
		}
	}
	return "自动选择"
}

func (a *app) desktopConfig(c *ui.Context) {
	ui.Fieldset(c, "虚拟显示器", func() {
		ui.Row(c).FillWidth().Wrap().Gap(16).Children(func() {
			ui.Checkbox(c, &a.cfg.FlexDisplay, "弹性尺寸").Tooltip("窗口大小改变时自动调整虚拟显示器分辨率（v4+）").OnChange(a.saveConfig)
			ui.Checkbox(c, &a.cfg.AspectLock, "比例锁").OnChange(a.saveConfig)
		})
		ui.Row(c).FillWidth().Wrap().Gap(8).AlignItems(ui.End).Children(func() {
			a.vdSlider(c, "宽度", &a.cfg.VdWidth, 480, 3840, a.lockHeightToWidth)
			a.vdSlider(c, "高度", &a.cfg.VdHeight, 360, 2160, a.lockWidthToHeight)
			ui.Field(c, "界面缩放 (DPI)", func() {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Slider(c, &a.cfg.VdDpi, 120, 640).Grow(1).OnChange(a.saveConfig)
					ui.Text(c, strconv.Itoa(int(a.cfg.VdDpi))+" DPI")
				})
			}).Grow(1).MinWidth(200)
		})
		ui.Field(c, "快速预设", func() {
			ui.Row(c).FillWidth().Wrap().Gap(6).Children(func() {
				for _, p := range []struct {
					label string
					w, h  float64
				}{
					{"1080p", 1920, 1080}, {"1440p", 2560, 1440}, {"4K", 3840, 2160}, {"带鱼屏", 2560, 1080},
				} {
					ui.Button(c, p.label).Grow(1).MinWidth(70).OnClick(func() {
						a.cfg.VdWidth = p.w
						a.cfg.VdHeight = p.h
						a.saveConfig()
					})
				}
			})
		})
	})
	ui.Field(c, "背景色", func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c, &a.cfg.BackgroundColor).Placeholder("#1a1a1a").Grow(1).OnChange(a.saveConfig)
			if a.cfg.BackgroundColor != "" {
				ui.Button(c, "清除").OnClick(func() {
					a.cfg.BackgroundColor = ""
					a.saveConfig()
				})
			}
		})
	})
	a.perfGrid(c, false, true)
}

func (a *app) vdSlider(c *ui.Context, label string, value *float64, lo, hi float64, lock func()) {
	ui.Field(c, label, func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Slider(c, value, lo, hi).Grow(1).OnChange(func() {
				lock()
				a.saveConfig()
			})
			ui.Text(c, strconv.Itoa(int(*value))+"px")
		})
	}).Grow(1).MinWidth(200)
}

func (a *app) lockHeightToWidth() {
	if !a.cfg.AspectLock || a.prevVdW <= 0 || a.prevVdH <= 0 {
		return
	}
	a.cfg.VdHeight = clampF(a.cfg.VdWidth*(a.prevVdH/a.prevVdW), 360, 2160)
}

func (a *app) lockWidthToHeight() {
	if !a.cfg.AspectLock || a.prevVdW <= 0 || a.prevVdH <= 0 {
		return
	}
	a.cfg.VdWidth = clampF(a.cfg.VdHeight*(a.prevVdW/a.prevVdH), 480, 3840)
}

func clampF(v, lo, hi float64) float64 {
	return min(max(float64(int(v)), lo), hi)
}

func (a *app) startStopButton(c *ui.Context) {
	if !a.sessionRunning() {
		label := "开始投屏"
		switch a.cfg.SessionMode {
		case modeCamera:
			label = "启动相机"
		case modeDesktop:
			label = "进入桌面"
		}
		if a.cfg.SessionMode == modeMirror && (a.cfg.HidKeyboard || a.cfg.HidMouse) && a.cfg.OtgPure {
			label = "初始化 OTG"
		}
		ui.PrimaryButton(c, label).Grow(1).OnClick(a.startSession)
		return
	}
	ui.Button(c, "结束会话").Grow(1).OnClick(a.stopSession)
}

func (a *app) drawSessionBehavior(c *ui.Context) {
	ui.Fieldset(c, "会话行为", func() {
		ui.Scroll(c).Fill().Children(func() {
			ui.Column(c).FillWidth().Gap(12).Children(func() {
				ui.Row(c).FillWidth().Wrap().Gap(16).Children(func() {
					ui.Checkbox(c, &a.cfg.StayAwake, "息屏保持唤醒").Tooltip("连接期间防止设备休眠").OnChange(a.saveConfig)
					ui.Checkbox(c, &a.cfg.KeepActive, "保持活跃").Tooltip("防止虚拟显示器超时休眠（v4+）").OnChange(a.saveConfig)
					ui.Checkbox(c, &a.cfg.TurnOff, "启动后关闭设备屏幕").Tooltip("镜像期间关闭设备物理屏幕并禁止唤醒").OnChange(a.saveConfig)
					ui.Checkbox(c, &a.cfg.AudioEnabled, "转发音频").OnChange(a.saveConfig)
					ui.Checkbox(c, &a.cfg.AlwaysOnTop, "窗口置顶").OnChange(a.saveConfig)
					ui.Checkbox(c, &a.cfg.Fullscreen, "全屏").OnChange(a.saveConfig)
					ui.Checkbox(c, &a.cfg.Borderless, "无边框").OnChange(a.saveConfig)
					ui.Checkbox(c, &a.cfg.Record, "录制画面").OnChange(a.saveConfig)
				})
				if a.cfg.AudioEnabled {
					codecSel := a.cfg.AudioCodec
					if codecSel == "" || codecSel == "auto" {
						codecSel = "自动"
					}
					ui.Field(c, "音频编解码器", func() {
						ui.Select(c, &codecSel, audioCodecs).OnChange(func() {
							if codecSel == "自动" {
								a.cfg.AudioCodec = "auto"
							} else {
								a.cfg.AudioCodec = codecSel
							}
							a.saveConfig()
						})
					}).Grow(1).MinWidth(150)
				}
				if a.cfg.Record {
					dir := a.cfg.RecordPath
					if dir == "" {
						dir = a.defaultRecordPath
					}
					ui.Field(c, "录制保存到", func() {
						ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
							ui.Text(c, dir).Grow(1).SingleLine().Ellipsis("…")
							ui.Button(c, "更改").OnClick(a.chooseRecordDir)
						})
					})
				}
			})
		})
	})
}

func (a *app) drawShortcutsModal(c *ui.Context) {
	a.modalPanel(c, &a.shortcutsOpen, "投屏快捷键", 360, func() {
		ui.Text(c, "在投屏窗口内使用，MOD 为 Alt 或 Super").TextColor(c.Theme().TextMuted)
		for _, s := range [][2]string{
			{"F", "全屏切换"}, {"H", "回到主屏"}, {"B", "返回"}, {"S", "最近任务"},
			{"P", "电源屏幕"}, {"R", "旋转屏幕"}, {"V", "粘贴剪贴板"}, {"O", "关闭设备屏幕"},
		} {
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Badge(c, s[0])
				ui.Text(c, s[1])
			})
		}
	})
}

func (a *app) modalPanel(c *ui.Context, open *bool, title string, width float32, fn func()) {
	ui.Modal(c, open, func() {
		ui.Column(c).Width(width).Gap(12).Children(func() {
			ui.Text(c, title).FontSize(15).Bold()
			fn()
		})
	})
}

func (a *app) drawOnboarding(c *ui.Context) {
	t := c.Theme()
	a.modalPanel(c, &a.onboardingOpen, "初始化核心组件", 480, func() {
		ready := a.scrcpyFound
		stateText := "scrcpy 未安装"
		color := t.Danger
		switch {
		case ready:
			stateText = "核心组件就绪"
			color = t.Success
		case a.isDownloading:
			stateText = "正在下载 " + formatPct(a.downloadPct)
			color = t.Accent
		}
		ui.Column(c).FillWidth().Gap(6).Children(func() {
			ui.Text(c, stateText).FontSize(13).Bold().TextColor(color)
			if a.isDownloading {
				ui.Progress(c, a.downloadPct/100)
			}
		})
		if !ready {
			if !a.isDownloading {
				ui.PrimaryButton(c, "下载核心组件").FillWidth().OnClick(a.downloadScrcpy)
				ui.Text(c, "也可以手动安装：从 GitHub Releases 下载对应平台压缩包，解压后通过顶栏「选择文件夹」指定目录。").FontSize(11).TextColor(t.TextMuted)
				ui.Link(c, "打开 scrcpy GitHub Releases", "https://github.com/Genymobile/scrcpy/releases")
				ui.Button(c, "稍后手动安装").FillWidth().OnClick(a.completeOnboarding)
				return
			}
			ui.Button(c, "下载中...").FillWidth().Disabled(true)
			ui.Button(c, "稍后手动安装").FillWidth().OnClick(a.completeOnboarding)
			return
		}
		ui.PrimaryButton(c, "进入应用").FillWidth().OnClick(a.completeOnboarding)
	})
}

func (a *app) drawGuideModal(c *ui.Context) {
	t := c.Theme()
	a.modalPanel(c, &a.guideOpen, "手动安装指南", 480, func() {
		for _, s := range []string{
			"1. 打开 GitHub Releases 页面，下载对应平台的 scrcpy 压缩包",
			"2. 解压压缩包到任意目录（例如 D:\\scrcpy）",
			"3. 点击顶栏「选择文件夹」，选中解压出的目录",
			"4. 确保目录中包含 scrcpy.exe 与 adb.exe",
		} {
			ui.Text(c, s).FontSize(12).TextColor(t.Text)
		}
		ui.Link(c, "前往 GitHub Releases", "https://github.com/Genymobile/scrcpy/releases")
		ui.PrimaryButton(c, "我知道了").FillWidth().OnClick(func() { a.guideOpen = false })
	})
}

func (a *app) openPairModal(target mdnsService) {
	path := a.cfg.ScrcpyPath
	a.pair.open = true
	a.pair.target = target
	a.pair.waiting = !strings.Contains(target.Service, "_adb-tls-pairing")
	a.pair.code = ""
	a.pair.err = ""
	if a.pair.waiting && !a.pair.polling {
		a.pair.polling = true
		go a.pollPairingBroadcast(path)
	}
}

func (a *app) pollPairingBroadcast(path string) {
	for {
		time.Sleep(2 * time.Second)
		stop := false
		a.onMain(func() {
			if !a.pair.open || !a.pair.waiting {
				a.pair.polling = false
				stop = true
				return
			}
			for _, s := range a.mdnsDevices {
				if s.Name == a.pair.target.Name && strings.Contains(s.Service, "_adb-tls-pairing") {
					a.pair.target = s
					a.pair.waiting = false
					a.pair.polling = false
					stop = true
					return
				}
			}
		})
		if stop {
			return
		}
		a.fetchDevicesOnce(path, true)
	}
}

func (a *app) drawPairModal(c *ui.Context) {
	t := c.Theme()
	a.modalPanel(c, &a.pair.open, "设备配对", 420, func() {
		ui.Text(c, a.pair.target.Name).FontSize(13).Bold().TextColor(t.Text)
		ui.Text(c, a.pair.target.Address).FontSize(11).TextColor(t.TextMuted)
		if a.pair.err != "" {
			ui.Text(c, a.pair.err).FontSize(11).TextColor(t.Danger)
		}
		if a.pair.waiting {
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Spinner(c)
				ui.Text(c, "等待设备广播配对码...").FontSize(12).TextColor(t.TextMuted)
			})
			ui.Text(c, "请在手机「无线调试 > 使用配对码配对设备」页面操作").FontSize(11).TextColor(t.TextMuted)
		} else {
			submit := func() {
				if a.pair.code != "" {
					a.pairWithCode(a.pair.target.Address, a.pair.code)
				}
			}
			ui.TextInput(c, &a.pair.code).Placeholder("输入 6 位配对码").AutoFocus().OnSubmit(submit)
			ui.PrimaryButton(c, "配对").FillWidth().OnClick(submit)
		}
		ui.Button(c, "取消").FillWidth().OnClick(func() {
			a.pair.open = false
			a.pair.err = ""
		})
	})
}

func (a *app) drawAlertDialog(c *ui.Context) {
	buttons := []string{"确定"}
	if a.alert.action != "" {
		buttons = []string{"取消", a.alert.action}
	}
	switch ui.AlertDialog(c, &a.alert.open, a.alert.title, a.alert.message, buttons...) {
	case 1:
		fn := a.alert.onAction
		a.closeAlert()
		if fn != nil {
			fn()
		}
	default:
		if !a.alert.open {
			a.closeAlert()
		}
	}
}

func newApp(st *dataStore, win *mygo.Window) *app {
	a := &app{
		cfg: config{
			SessionMode:  modeMirror,
			Bitrate:      8,
			AudioEnabled: true,
			AudioCodec:   "auto",
			Res:          "0",
			Codec:        "h264",
			VdWidth:      1920,
			VdHeight:     1080,
			VdDpi:        420,
			CameraZoom:   1.0,
			Vsync:        true,
		},
		deviceModels:      map[string]string{},
		friendlyNames:     map[string]string{},
		fetchedNames:      map[string]bool{},
		runningSet:        map[string]bool{},
		deviceSel:         -1,
		sidebarPercent:    30,
		consolePct:        80,
		scrcpyMessage:     "加载中...",
		defaultRecordPath: videosDir(),
		store:             st,
		win:               win,
	}
	a.cfg.RecordPath = a.defaultRecordPath
	if st != nil {
		st.load("config.json", &a.cfg)
		st.load("history.json", &a.history)
		state := struct {
			OnboardingDone bool `json:"onboardingDone"`
		}{}
		if st.load("state.json", &state) {
			a.onboardingDone = state.OnboardingDone
		}
	}
	a.r = newRunner(
		func(s string) { a.onMain(func() { a.appendLog(strings.Split(s, "\n")...) }) },
		a.onRunningChanged,
	)
	return a
}

func (a *app) onRunningChanged(device string, running bool) {
	a.onMain(func() {
		if running {
			a.runningSet[device] = true
		} else {
			delete(a.runningSet, device)
		}
	})
}

func (a *app) Init() {
	path := a.cfg.ScrcpyPath
	a.checkScrcpy(path)
	go a.refreshUntilSettled(path)
	go a.reconnectLoop()
	go a.devicePollLoop()
}

func (a *app) devicePollLoop() {
	type pollReq struct {
		skip bool
		path string
	}
	for {
		time.Sleep(5 * time.Second)
		ch := make(chan pollReq, 1)
		a.onMain(func() {
			if a.isRefreshing {
				ch <- pollReq{skip: true}
				return
			}
			ch <- pollReq{path: a.cfg.ScrcpyPath}
		})
		if req := <-ch; !req.skip {
			a.fetchDevicesOnce(req.path, true)
		}
	}
}

func (a *app) checkScrcpy(path string) {
	go func() {
		st := checkScrcpyBinary(path)
		supported, drivers := renderDrivers(path)
		a.onMain(func() {
			a.scrcpyFound = st.found
			a.scrcpyMessage = st.message
			a.renderSupported = supported
			a.renderDrivers = drivers
			if !st.found && !a.onboardingDone {
				a.onboardingOpen = true
			}
			if st.found && a.update.checkedPath != path {
				a.update.checkedPath = path
				go a.checkUpdate(path)
			}
		})
	}()
}

func (a *app) checkUpdate(path string) {
	res := checkScrcpyUpdate(path)
	a.onMain(func() {
		if !res.available {
			return
		}
		a.update.local = res.local
		a.update.latest = res.latest
		a.showAlert(
			"发现新版 scrcpy",
			fmt.Sprintf("当前版本 %s，最新版本 %s。是否立即更新？", res.local, res.latest),
			"立即更新",
			func() {
				a.cfg.ScrcpyPath = ""
				a.saveConfig()
				a.downloadScrcpy()
			},
		)
	})
}

func (a *app) fetchDevicesOnce(path string, silent bool) {
	res := adbDevices(path)
	var mdnsList []mdnsService
	if services, _ := mdnsServices(path); services != nil {
		for _, s := range services {
			if strings.Contains(s.Service, "_adb-tls-connect") || strings.Contains(s.Service, "_adb-tls-pairing") {
				mdnsList = append(mdnsList, s)
			}
		}
	}
	a.onMain(func() {
		a.mdnsDevices = mdnsList
		if res.Err != "" {
			if !silent {
				a.appendLog("设备发现出错: " + res.Err)
			}
			return
		}
		var added, removed []string
		for _, d := range res.Devices {
			if !slices.Contains(a.devices, d) {
				added = append(added, d)
			}
		}
		for _, d := range a.devices {
			if !slices.Contains(res.Devices, d) {
				removed = append(removed, d)
			}
		}
		for _, d := range added {
			a.appendLog("发现新设备: " + a.deviceLabel(d))
		}
		for _, d := range removed {
			a.appendLog("设备已断开: " + a.deviceLabel(d))
		}
		a.devices = res.Devices
		a.deviceModels = res.Models
		a.syncDeviceSelection()
		if !silent && len(added) == 0 && len(removed) == 0 {
			a.appendLog(fmt.Sprintf("设备发现进行中，已连接 %d 台", len(res.Devices)))
		}
		a.fetchFriendlyNames(path)
	})
}

func (a *app) syncDeviceSelection() {
	if i := slices.Index(a.devices, a.activeDevice); i >= 0 {
		a.deviceSel = i
		return
	}
	if len(a.devices) > 0 {
		a.activeDevice = a.devices[0]
		a.deviceSel = 0
		return
	}
	a.activeDevice = ""
	a.deviceSel = -1
}

func (a *app) fetchFriendlyNames(path string) {
	for _, serial := range a.devices {
		if a.fetchedNames[serial] {
			continue
		}
		a.fetchedNames[serial] = true
		go a.fetchFriendlyName(serial, path)
	}
}

var emulatorRe = regexp.MustCompile(`^emulator-\d+$`)

func (a *app) fetchFriendlyName(serial, path string) {
	cmd := "getprop ro.product.marketname; getprop ro.product.manufacturer; getprop ro.product.model"
	if emulatorRe.MatchString(serial) {
		cmd = "getprop ro.boot.qemu.avd_name; getprop ro.kernel.qemu.avd_name"
	}
	res := adbShell(serial, cmd, path)
	if !res.OK {
		return
	}
	name := friendlyNameFrom(res.Message, emulatorRe.MatchString(serial))
	if name == "" {
		return
	}
	a.onMain(func() { a.friendlyNames[serial] = name })
}

func friendlyNameFrom(out string, isEmulator bool) string {
	var trimmed []string
	for l := range strings.SplitSeq(strings.ReplaceAll(out, "\r", ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			trimmed = append(trimmed, l)
		}
	}
	if isEmulator {
		if len(trimmed) > 0 {
			return strings.ReplaceAll(trimmed[0], "_", " ")
		}
		return ""
	}
	if len(trimmed) == 0 {
		return ""
	}
	market, manufacturer, model := trimmed[0], "", ""
	if len(trimmed) > 1 {
		manufacturer = trimmed[1]
	}
	if len(trimmed) > 2 {
		model = trimmed[2]
	}
	switch {
	case market != "":
		return market
	case manufacturer != "" && model != "" && !strings.HasPrefix(strings.ToLower(model), strings.ToLower(manufacturer)):
		return manufacturer + " " + model
	}
	return model
}

func (a *app) deviceLabel(serial string) string {
	if n, ok := a.friendlyNames[serial]; ok && n != "" {
		return n
	}
	if m, ok := a.deviceModels[serial]; ok && m != "" {
		return m
	}
	return serial
}

func (a *app) refreshDevices() {
	if a.isRefreshing {
		return
	}
	a.isRefreshing = true
	path := a.cfg.ScrcpyPath
	go func() {
		a.fetchDevicesOnce(path, false)
		a.onMain(func() { a.isRefreshing = false })
	}()
}

func (a *app) refreshUntilSettled(path string) {
	started := make(chan bool, 1)
	a.onMain(func() {
		if a.isRefreshing {
			started <- false
			return
		}
		a.isRefreshing = true
		started <- true
	})
	if !<-started {
		return
	}
	defer a.onMain(func() { a.isRefreshing = false })
	prev := ""
	for i := range 9 {
		a.fetchDevicesOnce(path, true)
		time.Sleep(200 * time.Millisecond)
		ch := make(chan string, 1)
		a.onMain(func() { ch <- fmt.Sprintf("%v|%v", a.devices, a.mdnsDevices) })
		snapshot := <-ch
		if i > 0 && snapshot == prev {
			return
		}
		prev = snapshot
		if i < 8 {
			time.Sleep(2 * time.Second)
		}
	}
}

func (a *app) reconnectLoop() {
	for {
		time.Sleep(25 * time.Second)
		a.onMain(func() {
			for _, ip := range a.history {
				if !slices.Contains(a.devices, ip) {
					go adbConnect(ip, a.cfg.ScrcpyPath)
				}
			}
		})
	}
}

func (a *app) connectDevice(ip string) {
	if strings.TrimSpace(ip) == "" {
		return
	}
	a.isRefreshing = true
	path := a.cfg.ScrcpyPath
	go func() {
		res := adbConnect(ip, path)
		if !res.OK && (strings.Contains(res.Message, "failed to connect") || strings.Contains(res.Message, "cannot connect")) {
			a.onMain(func() { a.appendLog("连接失败，正在清除残留状态后重试...") })
			runTerminalCommand("", "adb disconnect "+ip, path)
			time.Sleep(500 * time.Millisecond)
			res = adbConnect(ip, path)
		}
		ok := res.OK
		a.onMain(func() {
			if ok {
				a.appendLog("已连接到 " + ip)
				a.addToHistory(ip)
			} else {
				msgs := []string{"连接失败: " + res.Message}
				if strings.Contains(res.Message, "failed to connect") || strings.Contains(res.Message, "cannot connect") {
					msgs = append(msgs, "提示: 无线调试端口每次连接都会变化，请在设备上重新查看端口")
				}
				a.appendLog(msgs...)
			}
			a.isRefreshing = false
		})
		if ok {
			time.Sleep(1 * time.Second)
			a.fetchDevicesOnce(path, true)
		}
	}()
}

func (a *app) pairWithCode(ip, code string) {
	path := a.cfg.ScrcpyPath
	go func() {
		a.onMain(func() { a.appendLog("[SYSTEM] 正在与 " + ip + " 配对...") })
		res := adbPair(ip, code, path)
		a.onMain(func() {
			if res.Message != "" {
				a.appendLog("[ADB] " + res.Message)
			}
			if res.OK {
				a.appendLog("配对成功: " + ip)
				a.pair.open = false
				a.pair.code = ""
				go a.refreshUntilSettled(path)
				return
			}
			a.pair.err = res.Message
			if strings.Contains(res.Message, "protocol fault") {
				a.appendLog("配对协议错误: 请确认设备仍停留在配对窗口，然后重试")
			}
		})
	}()
}

func (a *app) startSession() {
	if a.activeDevice == "" {
		a.showAlert("未选择设备", "请先在设备中心选择一台设备", "", nil)
		return
	}
	a.cfg.Device = a.activeDevice
	a.appendLog("正在为 " + a.deviceLabel(a.activeDevice) + " 初始化 scrcpy...")
	a.saveConfig()
	cfg := a.cfg
	go a.r.start(&cfg, a.defaultRecordPath)
}

func (a *app) stopSession() {
	if a.activeDevice == "" {
		return
	}
	device := a.activeDevice
	go a.r.stop(device)
}

func (a *app) downloadScrcpy() {
	if a.isDownloading {
		return
	}
	a.isDownloading = true
	a.downloadPct = 0
	dlPath := a.cfg.ScrcpyPath
	go func() {
		lastStatus := ""
		err := downloadScrcpyRelease(
			func(s string) { a.onMain(func() { a.appendLog(strings.Split(s, "\n")...) }) },
			func(p float64) { a.onMain(func() { a.downloadPct = p }) },
			func(msg string) { lastStatus = msg },
		)
		a.onMain(func() {
			a.isDownloading = false
			if err != nil {
				a.appendLog("下载失败: " + err.Error())
				return
			}
			a.appendLog("下载完成: " + lastStatus)
			go a.refreshUntilSettled(dlPath)
			a.checkScrcpy("")
		})
	}()
}

func (a *app) pushOrInstall(paths []string) {
	if a.activeDevice == "" {
		a.appendLog("没有选择设备，无法处理拖入的文件")
		return
	}
	device := a.activeDevice
	path := a.cfg.ScrcpyPath
	for _, p := range paths {
		go func(p string) {
			var res adbResult
			if strings.EqualFold(filepath.Ext(p), ".apk") {
				a.onMain(func() { a.appendLog("正在安装 APK: " + filepath.Base(p)) })
				res = adbInstall(device, p, path)
			} else {
				a.onMain(func() { a.appendLog("正在推送文件: " + filepath.Base(p)) })
				res = adbPush(device, p, path)
			}
			a.onMain(func() { a.appendLog("[ADB] " + res.Message) })
		}(p)
	}
}

func (a *app) browseFiles() {
	if a.activeDevice == "" {
		return
	}
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Multiple: true,
			Filters: []mygo.FileFilter{
				{Name: "所有文件", Extensions: []string{"*"}},
				{Name: "Android 应用 (APK)", Extensions: []string{"apk"}},
			},
		})
		if err != nil || len(paths) == 0 {
			return
		}
		a.onMain(func() { a.pushOrInstall(paths) })
	}()
}

func (a *app) chooseScrcpyFolder() {
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Directory:   true,
			DefaultPath: scrcpyBinDir(),
		})
		if err != nil || len(paths) == 0 {
			return
		}
		selected := paths[0]
		a.onMain(func() {
			a.cfg.ScrcpyPath = selected
			a.saveConfig()
			a.appendLog("已设置 scrcpy 自定义路径: " + selected)
			a.checkScrcpy(selected)
		})
	}()
}

func (a *app) resetScrcpyPath() {
	a.cfg.ScrcpyPath = ""
	a.saveConfig()
	a.appendLog("已清除自定义 scrcpy 路径")
	a.checkScrcpy("")
}

func (a *app) killAdb() {
	path := a.cfg.ScrcpyPath
	go func() {
		a.onMain(func() { a.appendLog("[SYSTEM] 正在终止 ADB 服务...") })
		killAdbServer(path)
		a.onMain(func() {
			a.appendLog("[SYSTEM] ADB 服务已终止")
			a.refreshDevices()
		})
	}()
}

func (a *app) listCameras() {
	if a.activeDevice == "" {
		return
	}
	device := a.activeDevice
	path := a.cfg.ScrcpyPath
	go func() {
		a.onMain(func() { a.appendLog("正在运行 scrcpy --list-cameras ...") })
		res := listScrcpyOptions(device, "--list-cameras", path)
		a.onMain(func() {
			a.appendLog(strings.Split(res.output, "\n")...)
			if cams := parseCameras(res.output); len(cams) > 0 {
				a.detectedCameras = cams
			} else {
				a.appendLog("未解析到任何相机")
			}
		})
	}()
}

func (a *app) runCommand(cmdLine string) {
	cmdLine = strings.TrimSpace(cmdLine)
	if cmdLine == "" {
		return
	}
	device := a.activeDevice
	path := a.cfg.ScrcpyPath
	prefix := ""
	if lower := strings.ToLower(cmdLine); !strings.HasPrefix(lower, "scrcpy") && !strings.HasPrefix(lower, "adb") {
		prefix = "adb "
	}
	a.appendLog("> " + prefix + cmdLine)
	go func() {
		res := runTerminalCommand(device, cmdLine, path)
		a.onMain(func() {
			if s := strings.TrimSpace(res.Stdout); s != "" {
				a.appendLog(strings.Split(s, "\n")...)
			}
			if s := strings.TrimSpace(res.Stderr); s != "" {
				tag := strings.ToUpper(res.Binary)
				if tag == "" {
					tag = "ERR"
				}
				for l := range strings.SplitSeq(s, "\n") {
					a.appendLog("[" + tag + "] " + l)
				}
			}
			if res.Err != "" {
				a.appendLog("命令执行失败: " + res.Err)
			}
		})
	}()
}

func (a *app) exportReport() {
	report := map[string]any{
		"timestamp": time.Now().Format(time.RFC3339),
		"config":    a.cfg,
		"history":   a.history,
		"logs":      a.logs,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		a.appendLog("导出报告失败: " + err.Error())
		return
	}
	name := fmt.Sprintf("scrcpy-gui-logs-%s.json", time.Now().Format("20060102_150405"))
	path := filepath.Join(userDataDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		a.appendLog("导出报告失败: " + err.Error())
		return
	}
	a.appendLog("报告已导出: " + path)
}

func (a *app) chooseRecordDir() {
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Directory:   true,
			DefaultPath: a.cfg.RecordPath,
		})
		if err != nil || len(paths) == 0 {
			return
		}
		a.onMain(func() {
			a.cfg.RecordPath = paths[0]
			a.saveConfig()
		})
	}()
}

func (a *app) completeOnboarding() {
	a.onboardingDone = true
	a.onboardingOpen = false
	if a.store != nil {
		_ = a.store.save("state.json", map[string]bool{"onboardingDone": true})
	}
}

func binaryPath(name, customFolder string) string {
	filename := name
	if runtime.GOOS == "windows" {
		filename = name + ".exe"
	}
	candidates := []string{customFolder, filepath.Join(userDataDir(), "scrcpy-bin"), "", ""}
	if exe, err := os.Executable(); err == nil {
		candidates[2] = filepath.Join(filepath.Dir(exe), "scrcpy-bin")
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates[3] = filepath.Join(cwd, "scrcpy-bin")
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		full := filepath.Join(dir, filename)
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			return full
		}
	}
	return name
}

func scrcpyBinDir() string {
	dir := filepath.Join(userDataDir(), "scrcpy-bin")
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return userDataDir()
}

func userDir(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, name)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return home
}

func videosDir() string { return userDir("Videos") }

var userDataDir = sync.OnceValue(func() string {
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil || dir == "" {
		return "."
	}
	return dir
})

func newCmd(program string, args ...string) *exec.Cmd {
	cmd := exec.Command(program, args...)
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = hideWindow()
	}
	return cmd
}

var knownAdbStates = [...]string{
	"device", "offline", "unauthorized", "authorizing", "unknown",
	"bootloader", "recovery", "sideload", "host", "connecting",
}

func parseDeviceLine(line string) (serial, state string, ok bool) {
	tokens := strings.Fields(line)
	for i, t := range tokens {
		if !slices.Contains(knownAdbStates[:], t) {
			continue
		}
		if i == 0 {
			return "", "", false
		}
		return strings.Join(tokens[:i], " "), t, true
	}
	return "", "", false
}

func extractModel(line string) string {
	for t := range strings.FieldsSeq(line) {
		if v, found := strings.CutPrefix(t, "model:"); found {
			if v = strings.ReplaceAll(v, "_", " "); v != "" {
				return v
			}
		}
	}
	return ""
}

func adbDevices(customPath string) deviceList {
	out, err := newCmd(binaryPath("adb", customPath), "devices", "-l").Output()
	if err != nil {
		return deviceList{Err: adbErrorMessage(err)}
	}
	res := deviceList{Models: map[string]string{}}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Scan()
	for sc.Scan() {
		line := sc.Text()
		serial, state, ok := parseDeviceLine(line)
		if !ok || state != "device" {
			continue
		}
		if m := extractModel(line); m != "" {
			res.Models[serial] = m
		}
		res.Devices = append(res.Devices, serial)
	}
	return res
}

func adbErrorMessage(err error) string {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return fmt.Sprintf("adb 返回错误(%d)", ee.ExitCode())
	}
	return err.Error()
}

func mdnsServices(customPath string) ([]mdnsService, string) {
	out, err := newCmd(binaryPath("adb", customPath), "mdns", "services").Output()
	if err != nil {
		return nil, "adb mdns 返回错误"
	}
	var services []mdnsService
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Scan()
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 3 {
			continue
		}
		s := mdnsService{
			Name:    strings.TrimSpace(parts[0]),
			Service: strings.TrimSpace(parts[1]),
			Address: strings.TrimSpace(parts[2]),
		}
		key := s.Name + "|" + s.Service + "|" + s.Address
		if !seen[key] {
			services = append(services, s)
			seen[key] = true
		}
	}
	return services, ""
}

func adbConnect(ip, customPath string) adbResult {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath("adb", customPath), "connect", ip)
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = hideWindow()
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return adbResult{false, "连接超时"}
	}
	if err != nil {
		return adbResult{false, err.Error()}
	}
	text := strings.TrimSpace(string(out))
	return adbResult{!strings.Contains(text, "cannot connect") && !strings.Contains(text, "failed"), text}
}

func adbPair(ip, code, customPath string) adbResult {
	out, err := newCmd(binaryPath("adb", customPath), "pair", ip, code).CombinedOutput()
	text := strings.TrimSpace(string(out))
	return adbResult{err == nil && strings.Contains(text, "Successfully paired"), text}
}

func adbShell(device, command, customPath string) adbResult {
	out, err := newCmd(binaryPath("adb", customPath), "-s", device, "shell", command).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return adbResult{false, err.Error()}
	}
	return adbResult{true, text}
}

func adbPush(device, filePath, customPath string) adbResult {
	out, err := newCmd(binaryPath("adb", customPath), "-s", device, "push", filePath, "/sdcard/Download/").CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return adbResult{false, msg}
		}
		return adbResult{false, "传输失败"}
	}
	return adbResult{true, "文件已推送到下载目录"}
}

func adbInstall(device, filePath, customPath string) adbResult {
	out, err := newCmd(binaryPath("adb", customPath), "-s", device, "install", filePath).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil && msg == "" {
		msg = "安装失败"
	}
	return adbResult{err == nil, msg}
}

func killAdbServer(customPath string) {
	_ = newCmd(binaryPath("adb", customPath), "kill-server").Run()
	if runtime.GOOS == "windows" {
		_ = newCmd("taskkill", "/F", "/IM", "adb.exe", "/T").Run()
		return
	}
	_ = newCmd("pkill", "adb").Run()
}

func splitArgs(s string) ([]string, bool) {
	var args []string
	var cur strings.Builder
	inQuotes := false
	for _, c := range s {
		switch {
		case c == '"':
			inQuotes = !inQuotes
		case unicodeSpace(c) && !inQuotes:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(c)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args, !inQuotes
}

func unicodeSpace(c rune) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func runTerminalCommand(device, cmdLine, customPath string) terminalResult {
	parts, ok := splitArgs(cmdLine)
	if !ok {
		parts = strings.Fields(cmdLine)
	}
	if len(parts) == 0 {
		return terminalResult{Err: "没有提供命令"}
	}
	binaryName := "adb"
	switch strings.ToLower(parts[0]) {
	case "scrcpy":
		binaryName = "scrcpy"
		parts = parts[1:]
	case "adb":
		parts = parts[1:]
	}
	var args []string
	globalAdb := binaryName == "adb" && len(parts) > 0 &&
		(parts[0] == "devices" || parts[0] == "connect" || parts[0] == "pair")
	if device != "" && !globalAdb && !slices.Contains(parts, "-s") && !slices.Contains(parts, "--serial") {
		args = append(args, "-s", device)
	}
	args = append(args, parts...)
	out, err := newCmd(binaryPath(binaryName, customPath), args...).Output()
	res := terminalResult{Binary: binaryName}
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			res.Stdout = string(out)
			res.Stderr = string(ee.Stderr)
			return res
		}
		res.Err = err.Error()
		return res
	}
	res.OK = true
	res.Stdout = string(out)
	return res
}

func resolveAudioCodec(cfg *config, override string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	c := strings.TrimSpace(cfg.AudioCodec)
	if c == "" || strings.EqualFold(c, "auto") {
		return ""
	}
	return c
}

func cameraSize(res string) string {
	if size, ok := map[string]string{
		"3840": "3840x2160", "2560": "2560x1440", "1920": "1920x1080", "1600": "1600x900",
		"1280": "1280x720", "1024": "1024x576", "800": "800x480",
	}[res]; ok {
		return size
	}
	return "1920x1080"
}

func buildScrcpyArgs(cfg *config, videoDirFallback, audioCodecOverride string) []string {
	var args []string
	if cfg.Device != "" {
		args = append(args, "-s", cfg.Device)
	}
	codec := cfg.Codec
	if codec == "" {
		codec = "h264"
	}
	args = append(args, "--video-codec="+codec)

	if cfg.SessionMode == modeMirror && (cfg.HidKeyboard || cfg.HidMouse) && cfg.OtgPure {
		if strings.Contains(cfg.Device, ".") || strings.Contains(cfg.Device, ":") {
			return append(args, "--no-video", "--no-audio", "--keyboard=uhid", "--mouse=uhid")
		}
		return append(args, "--otg")
	}

	if cfg.HidKeyboard {
		args = append(args, "--keyboard=uhid")
	}
	if cfg.HidMouse {
		args = append(args, "--mouse=uhid")
	}
	if d := strings.TrimSpace(cfg.RenderDriver); d != "" && d != "auto" {
		args = append(args, "--render-driver", d)
	}
	if cfg.Bitrate > 0 {
		args = append(args, "--video-bit-rate", strconv.Itoa(int(cfg.Bitrate))+"M")
	}
	if !cfg.AudioEnabled {
		args = append(args, "--no-audio")
	} else if c := resolveAudioCodec(cfg, audioCodecOverride); c != "" {
		args = append(args, "--audio-codec="+c)
	}
	for _, flag := range []struct {
		on bool
		v  string
	}{
		{cfg.AlwaysOnTop, "--always-on-top"},
		{cfg.Fullscreen, "--fullscreen"},
		{cfg.Borderless, "--window-borderless"},
	} {
		if flag.on {
			args = append(args, flag.v)
		}
	}
	if cfg.Rotation != "" && cfg.Rotation != "0" {
		args = append(args, "--orientation", cfg.Rotation)
	}
	if cfg.SessionMode != modeCamera {
		if cfg.StayAwake {
			args = append(args, "--stay-awake")
		}
		if cfg.KeepActive {
			args = append(args, "--keep-active")
		}
		if cfg.TurnOff {
			args = append(args, "--turn-screen-off", "--no-power-on")
		}
	}

	switch cfg.SessionMode {
	case modeCamera:
		args = append(args, "--video-source=camera")
		if cfg.CameraID != "" {
			args = append(args, "--camera-id="+cfg.CameraID)
		} else if cfg.CameraFacing != "" {
			args = append(args, "--camera-facing="+cfg.CameraFacing)
		}
		size := "1920x1080"
		if cfg.Res != "" && cfg.Res != "0" {
			size = cameraSize(cfg.Res)
		}
		args = append(args, "--camera-size="+size)
		if cfg.CameraAr != "" && cfg.CameraAr != "0" {
			args = append(args, "--camera-ar="+cfg.CameraAr)
		}
		if cfg.CameraHighSpeed {
			args = append(args, "--camera-high-speed")
		}
		if cfg.CameraTorch {
			args = append(args, "--camera-torch")
		}
		if cfg.CameraZoom > 1.005 {
			args = append(args, fmt.Sprintf("--camera-zoom=%.1f", cfg.CameraZoom))
		}
	case modeDesktop:
		w, h, dpi := orDefault(cfg.VdWidth, 1920), orDefault(cfg.VdHeight, 1080), orDefault(cfg.VdDpi, 420)
		args = append(args, fmt.Sprintf("--new-display=%dx%d/%d", int(w), int(h), int(dpi)), "--video-buffer=100")
		if cfg.FlexDisplay {
			args = append(args, "--flex-display")
		}
	}

	fps := 0
	if f, err := strconv.Atoi(strings.TrimSpace(cfg.FPS)); err == nil {
		fps = f
	}
	switch {
	case fps > 0 && cfg.SessionMode == modeCamera:
		args = append(args, "--camera-fps", strconv.Itoa(fps))
	case fps > 0:
		args = append(args, "--max-fps", strconv.Itoa(fps))
	case cfg.SessionMode == modeCamera && cfg.CameraHighSpeed:
		args = append(args, "--camera-fps", "60")
	}
	if cfg.SessionMode != modeCamera && cfg.Res != "" && cfg.Res != "0" {
		args = append(args, "--max-size", cfg.Res)
	}
	if cfg.Record {
		dir := cfg.RecordPath
		if strings.TrimSpace(dir) == "" {
			dir = orDefaultStr(videoDirFallback, ".")
		}
		filename := fmt.Sprintf("scrcpy_%s_%s.mkv",
			strings.ReplaceAll(cfg.Device, ":", "-"), time.Now().Format("20060102_150405"))
		args = append(args, "--record="+filepath.Join(dir, filename))
	}
	if c := strings.TrimSpace(cfg.BackgroundColor); c != "" {
		args = append(args, "--background-color="+c)
	}
	if cfg.IgnoreVideoEncoderConstraints {
		args = append(args, "--ignore-video-encoder-constraints")
	}
	return args
}

func orDefault(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func isAudioCodecError(text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "could not create default audio encoder") ||
		strings.Contains(lower, "failed to initialize audio") {
		return true
	}
	mentionsCodec := strings.Contains(lower, "audio encoder") || strings.Contains(lower, "audio codec")
	mentionsFailure := strings.Contains(lower, "fail") || strings.Contains(lower, "error") ||
		strings.Contains(lower, "could not") || strings.Contains(lower, "not available") ||
		strings.Contains(lower, "not supported")
	return mentionsCodec && mentionsFailure
}

func newRunner(log func(string), running func(device string, running bool)) *runner {
	return &runner{procs: map[string]*exec.Cmd{}, log: log, running: running}
}

func (r *runner) start(cfg *config, videoDir string) {
	exePath := binaryPath("scrcpy", cfg.ScrcpyPath)
	adbPath := binaryPath("adb", cfg.ScrcpyPath)

	mode := "屏幕镜像"
	switch cfg.SessionMode {
	case modeCamera:
		mode = "相机模式"
	case modeDesktop:
		mode = "桌面模式"
	}
	r.log("[SYSTEM] 正在启动" + mode + "会话...")
	resLabel := orDefaultStr(cfg.Res, "0")
	if resLabel == "0" {
		resLabel = "原始"
	}
	fpsLabel := orDefaultStr(cfg.FPS, "60")
	r.log(fmt.Sprintf("[SYSTEM] 目标: %s | 配置: %s @ %dMbps, %sfps",
		cfg.Device, resLabel, int(orDefault(cfg.Bitrate, 8)), fpsLabel))
	if cfg.Record {
		r.log("[SYSTEM] 已启用录制 -> 输出到 " + orDefaultStr(cfg.RecordPath, "视频目录"))
	}
	serverPath := ""
	if exePath != "scrcpy" {
		sp := filepath.Join(filepath.Dir(exePath), "scrcpy-server")
		if _, err := os.Stat(sp); err == nil {
			serverPath = sp
		}
	}
	r.log("[SYSTEM] 使用 scrcpy: " + exePath)
	r.log("[SYSTEM] 使用 adb: " + adbPath)
	go r.runLoop(cfg, exePath, adbPath, serverPath)
}

func (r *runner) spawn(cfg *config, exePath, adbPath, serverPath, audioCodecOverride string) (*exec.Cmd, *atomic.Bool, error) {
	args := buildScrcpyArgs(cfg, "", audioCodecOverride)
	r.log("> scrcpy " + strings.Join(args, " "))
	cmd := newCmd(exePath, args...)
	cmd.Env = append(os.Environ(), "ADB="+adbPath)
	if serverPath != "" {
		cmd.Env = append(cmd.Env, "SCRCPY_SERVER_PATH="+serverPath)
	}
	cmd.Env = append(cmd.Env, "SDL_RENDER_VSYNC="+map[bool]string{true: "1", false: "0"}[cfg.Vsync])
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	audioFlag := &atomic.Bool{}
	go streamLogs(stdout, r.log, nil)
	go streamLogs(stderr, r.log, audioFlag)
	return cmd, audioFlag, nil
}

func streamLogs(rd io.Reader, log func(string), audioFlag *atomic.Bool) {
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		if err := sc.Err(); err != nil {
			lines <- "[SYSTEM] 读取日志流失败: " + err.Error()
		}
	}()
	var buf []string
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				if len(buf) > 0 {
					log(strings.Join(buf, "\n"))
				}
				return
			}
			if audioFlag != nil && isAudioCodecError(l) {
				audioFlag.Store(true)
			}
			buf = append(buf, l)
			if len(buf) >= 50 {
				log(strings.Join(buf, "\n"))
				buf = buf[:0]
			}
		case <-ticker.C:
			if len(buf) > 0 {
				log(strings.Join(buf, "\n"))
				buf = buf[:0]
			}
		}
	}
}

func (r *runner) runLoop(cfg *config, exePath, adbPath, serverPath string) {
	cmd, audioFlag, err := r.spawn(cfg, exePath, adbPath, serverPath, "")
	if err != nil {
		r.log("[SYSTEM] 启动 scrcpy 失败: " + err.Error())
		return
	}
	r.register(cfg.Device, cmd)
	r.running(cfg.Device, true)

	chainIndex := 0
	autoFallback := cfg.shouldAutoFallback()
	for {
		if err := cmd.Wait(); err != nil {
			r.log("[SYSTEM] scrcpy 进程已退出: " + err.Error())
		} else {
			r.log("[SYSTEM] scrcpy 进程已正常退出")
		}
		r.unregister(cfg.Device, cmd)
		time.Sleep(250 * time.Millisecond)

		if autoFallback && audioFlag.Load() {
			if chainIndex < len(audioFallbackChain) {
				next := audioFallbackChain[chainIndex]
				r.log("[SYSTEM] 默认音频编码器失败，正在使用 " + strings.ToUpper(next) + " 重试...")
				newCmd_, newFlag, err := r.spawn(cfg, exePath, adbPath, serverPath, next)
				if err != nil {
					r.log("[SYSTEM] 重试启动失败: " + err.Error())
					break
				}
				cmd, audioFlag = newCmd_, newFlag
				r.register(cfg.Device, cmd)
				r.running(cfg.Device, true)
				chainIndex++
				continue
			}
			r.log("[SYSTEM] 未找到兼容的音频编码器，建议关闭音频转发。")
		}
		break
	}
	r.running(cfg.Device, false)
}

func (r *runner) register(device string, cmd *exec.Cmd) {
	r.mu.Lock()
	r.procs[device] = cmd
	r.mu.Unlock()
}

func (r *runner) unregister(device string, cmd *exec.Cmd) {
	r.mu.Lock()
	if cur, ok := r.procs[device]; ok && cur == cmd {
		delete(r.procs, device)
	}
	r.mu.Unlock()
}

func (r *runner) stop(device string) {
	r.mu.Lock()
	cmd := r.procs[device]
	delete(r.procs, device)
	r.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	if err := newCmd("taskkill", "/PID", pid).Run(); err != nil {
		_ = newCmd("kill", pid).Run()
	}
}

type scrcpyStatus struct {
	found   bool
	message string
}

func checkScrcpyBinary(customPath string) scrcpyStatus {
	if err := newCmd(binaryPath("scrcpy", customPath), "--version").Run(); err != nil {
		return scrcpyStatus{false, "未找到 scrcpy"}
	}
	return scrcpyStatus{true, "scrcpy 就绪"}
}

type listResult struct {
	ok     bool
	output string
}

func listScrcpyOptions(device, arg, customPath string) listResult {
	out, err := newCmd(binaryPath("scrcpy", customPath), "-s", device, arg).CombinedOutput()
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			return listResult{}
		}
	}
	return listResult{true, string(out)}
}

func parseCameras(output string) []camera {
	var cameras []camera
	for line := range strings.SplitSeq(output, "\n") {
		t := strings.TrimSpace(line)
		if id, details, ok := parseNewCamera(t); ok {
			cameras = append(cameras, camera{id, id + ": " + details})
			continue
		}
		if id, res, meta, ok := parseOldCamera(t); ok {
			if meta == "" {
				meta = "相机"
			}
			cameras = append(cameras, camera{id, id + ": " + meta + " (" + res + ")"})
		}
	}
	return cameras
}

func parseNewCamera(t string) (id, details string, ok bool) {
	rest, found := strings.CutPrefix(t, "--camera-id=")
	if !found {
		return "", "", false
	}
	id, rest, found = strings.Cut(rest, " ")
	if !found {
		return "", "", false
	}
	if d, found := strings.CutPrefix(strings.TrimSpace(rest), "("); found {
		if details, _, _ := strings.Cut(d, ")"); details != "" {
			return id, details, true
		}
	}
	return "", "", false
}

func parseOldCamera(t string) (id, res, meta string, ok bool) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "- "))
	if !strings.HasPrefix(s, "[") {
		return "", "", "", false
	}
	inner, rest, found := strings.Cut(s, "]")
	if !found {
		return "", "", "", false
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "(") {
		return "", "", "", false
	}
	r, tail, found := strings.Cut(strings.TrimPrefix(rest, "("), ")")
	if !found {
		return "", "", "", false
	}
	return strings.TrimPrefix(inner, "["), r, strings.TrimSpace(strings.TrimSuffix(tail, "\r")), true
}

var knownDrivers = [...]string{"direct3d", "opengl", "opengles2", "opengles", "metal", "software", "vulkan"}

func driverLabel(driver string) string {
	labels := map[string]string{
		"direct3d": "D3D11 (Direct3D)", "opengl": "OpenGL", "opengles2": "OpenGL ES 2",
		"opengles": "OpenGL ES", "metal": "Metal", "software": "Software", "vulkan": "Vulkan",
	}
	if l, ok := labels[driver]; ok {
		return l
	}
	return "自定义"
}

func driverAllowedOnOS(driver string) bool {
	hostOS := runtime.GOOS
	if hostOS == "darwin" {
		hostOS = "macos"
	}
	switch hostOS {
	case "windows":
		return driver != "metal"
	case "macos":
		return driver != "direct3d"
	case "linux":
		return driver != "direct3d" && driver != "metal"
	default:
		return true
	}
}

func renderDrivers(customPath string) (bool, []driverOption) {
	out, err := newCmd(binaryPath("scrcpy", customPath), "--help").CombinedOutput()
	if err != nil {
		return false, nil
	}
	combined := strings.ToLower(string(out))
	idx := strings.Index(combined, "--render-driver")
	if idx < 0 {
		return false, nil
	}
	context := combined[idx:min(idx+1600, len(combined))]
	var supported []driverOption
	for _, d := range knownDrivers {
		if strings.Contains(context, d) && driverAllowedOnOS(d) {
			supported = append(supported, driverOption{d, driverLabel(d)})
		}
	}
	return true, supported
}

func localScrcpyVersion(customPath string) string {
	out, err := newCmd(binaryPath("scrcpy", customPath), "--version").Output()
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		for i, f := range fields {
			if f == "scrcpy" && i+1 < len(fields) {
				return strings.TrimPrefix(fields[i+1], "v")
			}
		}
	}
	return ""
}

const (
	githubReleasesAPI    = "https://api.github.com/repos/Genymobile/scrcpy/releases/latest"
	githubReleasesLatest = "https://github.com/Genymobile/scrcpy/releases/latest"
)

func newGitHubRequest(url string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "my-scrcpy-go")
	return req, nil
}

func latestScrcpyVersion(client *http.Client) (string, error) {
	resp, err := doGitHubRequest(client, githubReleasesAPI)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		var body struct {
			TagName string `json:"tag_name"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) == nil && body.TagName != "" {
			return strings.TrimPrefix(body.TagName, "v"), nil
		}
	}
	resp2, err := client.Get(githubReleasesLatest)
	if err != nil {
		return "", err
	}
	resp2.Body.Close()
	parts := strings.Split(resp2.Request.URL.String(), "/")
	if len(parts) > 0 && strings.HasPrefix(parts[len(parts)-1], "v") {
		return strings.TrimPrefix(parts[len(parts)-1], "v"), nil
	}
	return "", errors.New("无法确定最新版本")
}

func doGitHubRequest(client *http.Client, url string) (*http.Response, error) {
	req, err := newGitHubRequest(url)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func compareVersions(local, remote string) bool {
	lp, rp := versionParts(local), versionParts(remote)
	for i := range max(len(lp), len(rp)) {
		l, r := 0, 0
		if i < len(lp) {
			l = lp[i]
		}
		if i < len(rp) {
			r = rp[i]
		}
		switch {
		case r > l:
			return true
		case l > r:
			return false
		}
	}
	return false
}

func versionParts(v string) []int {
	var out []int
	for part := range strings.SplitSeq(v, ".") {
		var digits strings.Builder
		for _, r := range part {
			if r < '0' || r > '9' {
				break
			}
			digits.WriteString(string(r))
		}
		if digits.String() == "" {
			continue
		}
		if n, err := strconv.Atoi(digits.String()); err == nil {
			out = append(out, n)
		}
	}
	return out
}

type updateCheck struct {
	available bool
	local     string
	latest    string
	message   string
}

func checkScrcpyUpdate(customPath string) updateCheck {
	local := localScrcpyVersion(customPath)
	if local == "" {
		return updateCheck{message: "scrcpy 未安装或不可用"}
	}
	client := &http.Client{Timeout: 15 * time.Second}
	latest, err := latestScrcpyVersion(client)
	if err != nil {
		return updateCheck{local: local, message: "无法获取最新版本: " + err.Error()}
	}
	return updateCheck{available: compareVersions(local, latest), local: local, latest: latest}
}

func platformTags() (archTag, ext string, err error) {
	switch runtime.GOOS {
	case "windows":
		if runtime.GOARCH == "amd64" {
			return "win64", ".zip", nil
		}
		return "win32", ".zip", nil
	case "linux":
		return "linux-x86_64", ".tar.gz", nil
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "macos-aarch64", ".tar.gz", nil
		}
		return "macos-x86_64", ".tar.gz", nil
	}
	return "", "", errors.New("不支持自动下载的操作系统")
}

func resolveDownloadURL(client *http.Client, archTag, ext string, log func(string)) (string, string, error) {
	resp, err := doGitHubRequest(client, githubReleasesAPI)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		var body struct {
			Assets []struct {
				Name               string `json:"name"`
				BrowserDownloadURL string `json:"browser_download_url"`
			} `json:"assets"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) == nil {
			for _, asset := range body.Assets {
				if strings.Contains(asset.Name, archTag) && strings.HasSuffix(asset.Name, ext) {
					return asset.BrowserDownloadURL, asset.Name, nil
				}
			}
		}
	} else {
		log("[SYSTEM] API 受限，尝试备用发现方式...")
	}
	resp2, err := client.Get(githubReleasesLatest)
	if err != nil {
		return "", "", fmt.Errorf("备用方式失败: %w", err)
	}
	resp2.Body.Close()
	parts := strings.Split(resp2.Request.URL.String(), "/")
	if len(parts) == 0 || !strings.HasPrefix(parts[len(parts)-1], "v") {
		return "", "", errors.New("无法发现最新版本标签")
	}
	tag := parts[len(parts)-1]
	filename := fmt.Sprintf("scrcpy-%s-%s%s", archTag, tag, ext)
	log("[SYSTEM] 通过备用方式发现最新标签: " + tag)
	return fmt.Sprintf("https://github.com/Genymobile/scrcpy/releases/download/%s/%s", tag, filename), filename, nil
}

func downloadScrcpyRelease(log func(string), progress func(float64), status func(string)) error {
	archTag, ext, err := platformTags()
	if err != nil {
		return err
	}
	log("[SYSTEM] 检测到平台: " + archTag)
	status("正在获取最新发布版本...")

	client := &http.Client{Timeout: 10 * time.Minute}
	downloadURL, filename, err := resolveDownloadURL(client, archTag, ext, log)
	if err != nil {
		return err
	}
	if downloadURL == "" {
		return fmt.Errorf("未找到 %s 二进制（可能触发了 API 限流）", archTag)
	}
	log("[SYSTEM] 找到资源: " + filename)

	tempRoot, err := os.MkdirTemp("", "my-scrcpy-go-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempRoot)
	tempArchive := filepath.Join(tempRoot, "scrcpy_download"+ext)
	tempExtract := filepath.Join(tempRoot, "extract")
	extractPath := filepath.Join(userDataDir(), "scrcpy-bin")

	resp, err := client.Get(downloadURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("下载失败: HTTP " + resp.Status)
	}
	total := resp.ContentLength
	log(fmt.Sprintf("[SYSTEM] 正在下载: %d MB", total/1024/1024))
	if err := downloadToFile(resp.Body, tempArchive, total, progress); err != nil {
		return err
	}

	log("[SYSTEM] 下载完成，开始解压...")
	status("正在解压...")
	_ = os.RemoveAll(extractPath)
	if ext == ".zip" {
		log("[SYSTEM] 正在解压 ZIP 压缩包...")
		err = extractZip(tempArchive, tempExtract)
	} else {
		log("[SYSTEM] 正在解压 TAR.GZ 压缩包...")
		err = extractTarGz(tempArchive, tempExtract)
	}
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(tempExtract)
	if err != nil {
		return err
	}
	src := tempExtract
	if len(entries) > 0 && entries[0].IsDir() {
		src = filepath.Join(tempExtract, entries[0].Name())
	}
	if err := os.Rename(src, extractPath); err != nil {
		if err := copyDir(src, extractPath); err != nil {
			return err
		}
	}
	status(extractPath)
	return nil
}

func downloadToFile(body io.Reader, path string, total int64, progress func(float64)) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var downloaded int64
	buf := make([]byte, 64*1024)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			downloaded += int64(n)
			if total > 0 {
				progress(float64(downloaded) * 100 / float64(total))
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

func extractZip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		target := filepath.Join(dst, f.Name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := extractZipFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

func extractZipFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

func extractTarGz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dst, hdr.Name)
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeTarFile(tr, target); err != nil {
			return err
		}
	}
}

func writeTarFile(tr *tar.Reader, target string) error {
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, tr)
	return err
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, 0o755); err != nil {
			return err
		}
	}
	return nil
}

type dataStore struct {
	mu  sync.Mutex
	dir string
}

func newDataStore() *dataStore {
	return &dataStore{dir: userDataDir()}
}

func (s *dataStore) load(name string, v any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	return err == nil && json.Unmarshal(data, v) == nil
}

func (s *dataStore) save(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
