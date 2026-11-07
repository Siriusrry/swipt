package accessibility

import (
	"context"
	"errors"
	"testing"

	"github.com/swipt-automation/swipt/internal/coredevice"
)

type foregroundFixture struct {
	locks       []bool
	lockReads   int
	lockError   error
	backlight   string
	displayErr  error
	processes   [][]uint64
	processRead int
	processErr  error
	apps        map[uint64]frontmostApplication
	appErrors   map[uint64]error
}

func (f *foregroundFixture) LockState(context.Context) (coredevice.LockState, error) {
	index := min(f.lockReads, len(f.locks)-1)
	f.lockReads++
	return coredevice.LockState{PasscodeRequired: f.locks[index]}, f.lockError
}

func (f *foregroundFixture) BacklightState(context.Context) (string, error) {
	return f.backlight, f.displayErr
}

func (f *foregroundFixture) ForegroundProcesses(context.Context) ([]uint64, error) {
	index := min(f.processRead, len(f.processes)-1)
	f.processRead++
	return f.processes[index], f.processErr
}

func app(pid uint64, bundle string) frontmostApplication {
	return frontmostApplication{element: Element{Ref: ElementRef{PID: pid}}, bundle: bundle}
}

func TestRecoverStaleLockScreen(t *testing.T) {
	original := app(10, sleepLockScreen)
	visible := app(20, "com.example.visible")
	other := app(30, "com.example.other")
	failure := errors.New("service unavailable")
	tests := []struct {
		name   string
		change func(*foregroundFixture)
		want   frontmostApplication
	}{
		{name: "unlocked with no cached foreground", want: visible},
		{name: "dimmed display", change: func(f *foregroundFixture) { f.backlight = "dimmed" }, want: visible},
		{name: "locked", change: func(f *foregroundFixture) { f.locks = []bool{true} }, want: original},
		{name: "lock state unavailable", change: func(f *foregroundFixture) { f.lockError = failure }, want: original},
		{name: "always on display", change: func(f *foregroundFixture) { f.backlight = "inactiveOn" }, want: original},
		{name: "display off", change: func(f *foregroundFixture) { f.backlight = "off" }, want: original},
		{name: "display unavailable", change: func(f *foregroundFixture) { f.displayErr = failure }, want: original},
		{name: "process list unavailable", change: func(f *foregroundFixture) { f.processErr = failure }, want: original},
		{name: "only lock screen", change: func(f *foregroundFixture) { f.processes = [][]uint64{{10}} }, want: original},
		{name: "two foreground apps", change: func(f *foregroundFixture) { f.processes = [][]uint64{{20, 30}} }, want: original},
		{name: "process changed during read", change: func(f *foregroundFixture) { f.processes = [][]uint64{{20}, {30}} }, want: original},
		{name: "device locked during read", change: func(f *foregroundFixture) { f.locks = []bool{false, true} }, want: original},
		{name: "PID mismatch", change: func(f *foregroundFixture) { f.apps[20] = app(30, visible.bundle) }, want: original},
		{name: "missing bundle ID", change: func(f *foregroundFixture) { f.apps[20] = app(20, "") }, want: original},
		{name: "another lock screen process", change: func(f *foregroundFixture) { f.apps[20] = app(20, sleepLockScreen) }, want: original},
		{name: "foreground process exited", change: func(f *foregroundFixture) { f.appErrors[20] = ErrNoApplication }, want: original},
		{name: "application query failed", change: func(f *foregroundFixture) { f.appErrors[20] = failure }, want: original},
		{name: "SpringBoard alongside app", change: func(f *foregroundFixture) {
			f.processes = [][]uint64{{10, 20, 40}}
			f.apps[40] = app(40, "com.apple.springboard")
		}, want: visible},
		{name: "home screen", change: func(f *foregroundFixture) {
			f.processes = [][]uint64{{10, 40}}
			f.apps[40] = app(40, "com.apple.springboard")
		}, want: app(40, "com.apple.springboard")},
		{name: "process ordering changed", change: func(f *foregroundFixture) {
			f.processes = [][]uint64{{10, 20, 20}, {20, 10}}
		}, want: visible},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := &foregroundFixture{
				locks: []bool{false}, backlight: "activeOn", processes: [][]uint64{{10, 20}},
				apps: map[uint64]frontmostApplication{20: visible, 30: other}, appErrors: make(map[uint64]error),
			}
			if test.change != nil {
				test.change(fixture)
			}
			result := recoverLockScreen(context.Background(), original, &foregroundRecovery{
				lock: fixture, display: fixture, processes: fixture.ForegroundProcesses,
				application: func(_ context.Context, pid uint64) (frontmostApplication, error) {
					return fixture.apps[pid], fixture.appErrors[pid]
				},
			})
			if result.element.Ref.PID != test.want.element.Ref.PID || result.bundle != test.want.bundle {
				t.Fatalf("got %s (pid %d), want %s (pid %d)", result.bundle, result.element.Ref.PID, test.want.bundle, test.want.element.Ref.PID)
			}
			if fixture.locks[0] && fixture.processRead != 0 {
				t.Fatal("queried foreground processes while locked")
			}
		})
	}
}
