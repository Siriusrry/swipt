package accessibility

import (
	"context"
	"errors"
	"slices"

	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/coredevice"
	"github.com/swipt-automation/swipt/internal/coredevice/apps"
	"github.com/swipt-automation/swipt/internal/coredevice/display"
	"github.com/swipt-automation/swipt/internal/coredevice/instruments"
)

const sleepLockScreen = "com.apple.SleepLockScreen"

type frontmostApplication struct {
	element Element
	bundle  string
}

// Frontmost resolves the application used by both foreground tracking and tree
// reads. A corrected tracker alone cannot fix Find: it otherwise asks AX for the
// same stale frontmost process again.
func (c *RemoteClient) Frontmost(ctx context.Context) (Element, string, error) {
	batch := &Batch{}
	front := batch.Frontmost()
	if err := c.Run(ctx, batch); err != nil {
		return Element{}, "", err
	}
	if err := front.Err("read the frontmost application"); err != nil {
		return Element{}, "", err
	}
	resolved, err := c.resolveFrontmost(ctx, front.Value)
	return resolved.element, resolved.bundle, err
}

func (c *RemoteClient) resolveFrontmost(ctx context.Context, element Element) (frontmostApplication, error) {
	batch := &Batch{}
	bundle := batch.BundleID(element)
	if err := c.Run(ctx, batch); err != nil {
		return frontmostApplication{}, err
	}
	if err := bundle.Err("read the frontmost application's bundle ID"); err != nil {
		return frontmostApplication{}, c.silent(ctx, element, false, err)
	}
	front := frontmostApplication{element: element, bundle: bundle.Value}
	if front.bundle != sleepLockScreen || c.device == nil {
		return front, nil
	}

	// iPhone13,1 / iOS 27.3 (24C5037i): AX reported SleepLockScreen after
	// unlocking, while Instruments reported the visible app and Application(pid)
	// returned its tree. Do not use this fallback on an actually locked device.
	lock, err := c.device.DialLockState(ctx)
	if err != nil {
		return front, nil
	}
	defer lock.Close()
	state, err := lock.LockState(ctx)
	if err != nil || state.PasscodeRequired {
		return front, ctx.Err()
	}
	installed, err := apps.List(ctx, c.device)
	if err != nil {
		return front, ctx.Err()
	}
	bundles := []string{"com.apple.springboard"}
	for _, app := range installed {
		bundles = append(bundles, app.BundleID)
	}
	info, err := instruments.DialDeviceInfo(ctx, c.ctx, c.device)
	if err != nil {
		return front, nil
	}
	defer info.Close()
	return recoverLockScreen(ctx, front, &foregroundRecovery{
		lock:    lock,
		display: display.New(c.ctx, c.device, logger.NOP()),
		processes: func(ctx context.Context) ([]uint64, error) {
			return info.ForegroundApplications(ctx, bundles)
		},
		application: func(ctx context.Context, pid uint64) (frontmostApplication, error) {
			batch := &Batch{}
			app := batch.Application(pid)
			if err := c.Run(ctx, batch); err != nil {
				return frontmostApplication{}, err
			}
			if err := app.Err("read foreground process"); err != nil {
				return frontmostApplication{}, err
			}
			batch = &Batch{}
			bundle := batch.BundleID(app.Value)
			if err := c.Run(ctx, batch); err != nil {
				return frontmostApplication{}, err
			}
			return frontmostApplication{element: app.Value, bundle: bundle.Value}, bundle.Err("read foreground process bundle ID")
		},
	}), ctx.Err()
}

type foregroundRecovery struct {
	lock interface {
		LockState(context.Context) (coredevice.LockState, error)
	}
	display interface {
		BacklightState(context.Context) (string, error)
	}
	processes   func(context.Context) ([]uint64, error)
	application func(context.Context, uint64) (frontmostApplication, error)
}

func recoverLockScreen(ctx context.Context, original frontmostApplication, source *foregroundRecovery) frontmostApplication {
	state, err := source.lock.LockState(ctx)
	if err != nil || state.PasscodeRequired {
		return original
	}
	backlight, err := source.display.BacklightState(ctx)
	if err != nil || backlight != "activeOn" && backlight != "dimmed" {
		return original
	}
	pids, err := source.processes(ctx)
	if err != nil {
		return original
	}
	var candidate, springboard frontmostApplication
	seen := make(map[uint64]bool)
	for _, pid := range pids {
		if pid == 0 || pid == original.element.Ref.PID || seen[pid] {
			continue
		}
		seen[pid] = true
		app, err := source.application(ctx, pid)
		if errors.Is(err, ErrNoApplication) || errors.Is(err, ErrNoAnswer) {
			continue
		}
		if err != nil {
			return original
		}
		if app.element.Ref.PID != pid || app.bundle == "" || app.bundle == sleepLockScreen {
			return original
		}
		if app.bundle == "com.apple.springboard" {
			springboard = app
			continue
		}
		if candidate.element.Ref.PID != 0 {
			return original
		}
		candidate = app
	}
	if candidate.element.Ref.PID == 0 {
		candidate = springboard
	}
	if candidate.element.Ref.PID == 0 {
		return original
	}
	// Do not return a process selected before a lock or app-switch transition.
	latest, err := source.processes(ctx)
	if err != nil || !slices.Equal(sortedPIDs(pids), sortedPIDs(latest)) {
		return original
	}
	state, err = source.lock.LockState(ctx)
	if err != nil || state.PasscodeRequired {
		return original
	}
	return candidate
}

func sortedPIDs(pids []uint64) []uint64 {
	result := slices.Clone(pids)
	slices.Sort(result)
	return slices.Compact(result)
}
