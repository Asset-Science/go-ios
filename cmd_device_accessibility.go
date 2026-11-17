package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/accessibility"
)

func runAssistiveTouchCommand(ctx commandContext) {
	runAccessibilityToggle(ctx, assistiveTouch)
}

func runVoiceOverCommand(ctx commandContext) {
	runAccessibilityToggle(ctx, voiceOver)
}

func runZoomCommand(ctx commandContext) {
	runAccessibilityToggle(ctx, zoomTouch)
}

func runAccessibilityToggle(ctx commandContext, run func(ios.DeviceEntry, string, bool)) {
	force, _ := ctx.Args.Bool("--force")
	for _, operation := range []string{"enable", "disable", "toggle", "get"} {
		if enabled, _ := ctx.Args.Bool(operation); enabled {
			run(ctx.Device, operation, force)
		}
	}
}

func runTimeFormatCommand(ctx commandContext) {
	force, _ := ctx.Args.Bool("--force")
	for _, operation := range []string{"24h", "12h", "toggle", "get"} {
		if enabled, _ := ctx.Args.Bool(operation); enabled {
			timeFormat(ctx.Device, operation, force)
		}
	}
}

func runAXCommand(ctx commandContext) {
	if audit, _ := ctx.Args.Bool("audit"); audit {
		runAxAudit(ctx.Device)
		return
	}
	if find, _ := ctx.Args.Bool("find"); find {
		runAxFind(ctx)
		return
	}
	startAx(ctx.Device, ctx.Args)
}

func runResetAXCommand(ctx commandContext) {
	resetAx(ctx.Device)
}

// runAxFind is `ios ax find`: it reads the screen as text until a label matches --pattern.
// DESK-2464: PDD reads the EID off Settings > General > About this way, with no screenshot.
func runAxFind(ctx commandContext) {
	pattern, _ := ctx.Args.String("--pattern")
	re, err := regexp.Compile(pattern)
	exitIfError("invalid --pattern", err)
	opts := accessibility.FindOptions{}
	if s, _ := ctx.Args.String("--seconds"); s != "" {
		seconds, err := strconv.Atoi(s)
		if err != nil || seconds <= 0 {
			exitIfError("invalid --seconds", fmt.Errorf("want a positive number of seconds, got %q", s))
		}
		opts.Wait = time.Duration(seconds) * time.Second
	}
	if s, _ := ctx.Args.String("--max-elements"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			exitIfError("invalid --max-elements", fmt.Errorf("want a positive number, got %q", s))
		}
		opts.MaxElements = n
	}
	result, err := accessibility.FindOnScreen(context.Background(), ctx.Device, re, opts)
	exitIfError("ax find failed", err)
	fmt.Println(convertToJSONString(result))
}
