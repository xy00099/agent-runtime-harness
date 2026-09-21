// Package android implements the Android SDK adapter (roadmap v0.2: prove
// the abstraction is not Unity-specific).
//
// Design mirrors the Unity adapter's contract: the adapter resolves tools
// from the registry, requests leases through the daemon bundle, and never
// owns global runtime state.
//
// Resource model (roadmap §26 "simulator/device leases"):
//
//	adb:<serial>   exclusive   a physical device or emulator serial,
//	                             discovered dynamically from `adb devices`
//	android-emulator-slot   capacity   concurrent emulator boots
package android

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/adapter"
	"github.com/xy00099/agent-runtime-harness/internal/model"
)

// Adapter runs Android SDK commands.
type Adapter struct{}

// New builds the adapter.
func New() *Adapter { return &Adapter{} }

// Type implements adapter.ToolAdapter.
func (a *Adapter) Type() string { return "android" }

// Capabilities implements adapter.ToolAdapter.
func (a *Adapter) Capabilities() []string {
	return []string{
		"devices",       // list adb devices (no lease)
		"emulators",     // list AVDs (no lease)
		"targets",       // list installed SDK targets (no lease)
		"build.apk",     // build a debug APK from source dir (no device needed)
		"avd.create",    // create an AVD in the session's isolated ANDROID_AVD_HOME
		"emulator.boot", // boot an AVD under supervision (holds an emulator slot)
		"install",       // install an APK onto a leased device
		"shell",         // run a shell command on a leased device
		"screenshot",    // capture a device screenshot artifact
		"uninstall",     // remove a package from a leased device
	}
}

// needsLease maps commands to their dynamic lease resources. Device
// commands lease the specific adb serial exclusively.
func needsDeviceLease(command string) bool {
	switch command {
	case "install", "shell", "screenshot", "uninstall":
		return true
	}
	return false
}

// sdk returns the resolved SDK root from the tool env.
func sdkRoot(tool *model.Tool) string {
	if v, ok := tool.Env["ANDROID_HOME"]; ok && v != "" {
		return v
	}
	if v, ok := tool.Env["ANDROID_SDK_ROOT"]; ok && v != "" {
		return v
	}
	return ""
}

// batName names a Windows batch wrapper; on unix the same tool is a
// shell script without an extension.
func batName(name string) string {
	if runtimeIsWindowsFS() {
		return name + ".bat"
	}
	return name
}

// runtimeIsWindowsFS reports a Windows filesystem separator.
func runtimeIsWindowsFS() bool { return filepath.Separator == 92 }

// adbPath returns the adb executable path for the tool.
func adbPath(tool *model.Tool) (string, error) {
	if v, ok := tool.Env["ARH_ADB"]; ok && v != "" {
		return v, nil
	}
	root := sdkRoot(tool)
	if root == "" {
		return "", fmt.Errorf("android adapter: tool env needs ANDROID_HOME (SDK root)")
	}
	adb := filepath.Join(root, "platform-tools", exeName("adb"))
	if _, err := os.Stat(adb); err != nil {
		return "", fmt.Errorf("android adapter: adb not found at %s", adb)
	}
	return adb, nil
}

func exeName(name string) string {
	if filepath.Separator == '\\' {
		return name + ".exe"
	}
	return name
}

// Prepare implements adapter.ToolAdapter.
func (a *Adapter) Prepare(ctx context.Context, req *adapter.Request) ([]string, error) {
	if req.Tool == nil {
		return nil, fmt.Errorf("android adapter: no tool resolved")
	}
	if !a.knownCommand(req.Command) {
		return nil, fmt.Errorf("android adapter: unknown command %q (have %v)", req.Command, a.Capabilities())
	}
	if needsDeviceLease(req.Command) {
		serial := strArg(req.Args, "serial", strArg(req.Args, "device", ""))
		if serial == "" {
			return nil, fmt.Errorf("android adapter: %s requires serial/device (the adb serial to lease)", req.Command)
		}
		// Dynamic resource: the daemon registers adb:<serial> on demand.
		return []string{"adb:" + serial}, nil
	}
	if req.Command == "emulator.boot" {
		return []string{"android-emulator-slot"}, nil
	}
	return nil, nil
}

func (a *Adapter) knownCommand(c string) bool {
	for _, x := range a.Capabilities() {
		if x == c {
			return true
		}
	}
	return false
}

// Launch implements adapter.ToolAdapter.
func (a *Adapter) Launch(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string) (*adapter.Result, error) {
	tool := req.Tool
	sess := req.Session

	// Acquire declared leases (device commands hold them for the run).
	needed, err := a.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	for _, rid := range needed {
		if _, err := rt.AcquireLease(ctx, sess.ID, rid, 2*time.Hour); err != nil {
			return nil, fmt.Errorf("acquire %s: %w", rid, err)
		}
	}

	// Child env: session env + tool env (SDK paths).
	env := append([]string{}, req.SessionEnv...)
	for k, v := range tool.Env {
		env = append(env, k+"="+v)
	}

	switch req.Command {
	case "devices":
		return a.runAdb(ctx, req, rt, runDir, env, "devices", "-l")
	case "emulators":
		return a.listAVDs(ctx, req, rt, runDir, env)
	case "targets":
		return a.listTargets(ctx, req, rt, runDir, env)
	case "build.apk":
		return a.buildAPK(ctx, req, rt, runDir, env)
	case "avd.create":
		return a.createAVD(ctx, req, rt, runDir, env)
	case "emulator.boot":
		return a.bootEmulator(ctx, req, rt, runDir, env)
	case "install", "shell", "screenshot", "uninstall":
		return a.deviceCommand(ctx, req, rt, runDir, env)
	}
	return nil, fmt.Errorf("android adapter: unhandled command %q", req.Command)
}

// runAdb runs a bare adb subcommand.
func (a *Adapter) runAdb(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string, env []string, args ...string) (*adapter.Result, error) {
	adb, err := adbPath(req.Tool)
	if err != nil {
		return nil, err
	}
	rt.Log("", "info", fmt.Sprintf("adb %v", args))
	return a.exec(ctx, req, rt, runDir, adb, args, env, rt.WorkspaceDir(req.Session.ID))
}

// exec is the supervised launch used by every path.
func (a *Adapter) exec(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir, exe string, args, env []string, dir string) (*adapter.Result, error) {
	timeout := a.timeoutFor(req.Command)
	if req.Timeout > 0 {
		timeout = req.Timeout // caller override (CLI --timeout-min, MCP)
	}
	res, err := rt.Exec(ctx, adapter.ExecRequest{
		SessionID: req.Session.ID,
		Label:     fmt.Sprintf("android:%s", req.Command),
		Exe:       exe,
		Args:      args,
		Env:       env,
		Dir:       dir,
		RunDir:    runDir,
		Timeout:   timeout,
	})
	if err != nil {
		return nil, err
	}
	return &adapter.Result{ExitCode: res.ExitCode, Status: res.Status, Note: res.Note}, nil
}

func (a *Adapter) timeoutFor(command string) time.Duration {
	switch command {
	case "emulator.boot":
		return 10 * time.Minute
	case "build.apk":
		return 30 * time.Minute
	default:
		return 5 * time.Minute
	}
}

// listAVDs enumerates AVDs visible to the session's isolated AVD home.
func (a *Adapter) listAVDs(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string, env []string) (*adapter.Result, error) {
	emu, err := emulatorPath(req.Tool)
	if err != nil {
		return nil, err
	}
	rt.Log("", "info", "emulator -list-avds")
	return a.exec(ctx, req, rt, runDir, emu, []string{"-list-avds"}, env, rt.WorkspaceDir(req.Session.ID))
}

// listTargets lists installed SDK platforms.
func (a *Adapter) listTargets(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string, env []string) (*adapter.Result, error) {
	root := sdkRoot(req.Tool)
	if root == "" {
		return nil, fmt.Errorf("android adapter: ANDROID_HOME not set on tool")
	}
	platforms := filepath.Join(root, "platforms")
	entries, err := os.ReadDir(platforms)
	if err != nil {
		return nil, fmt.Errorf("list platforms: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "android-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := "targets:\n" + strings.Join(prefixEach(names, "  - "), "\n") + "\n"
	if err := os.WriteFile(filepath.Join(runDir, "targets.txt"), []byte(out), 0o644); err != nil {
		return nil, err
	}
	code := 0
	return &adapter.Result{ExitCode: &code, Status: model.RunSucceeded,
		ExtraFiles: map[string]string{"targets": filepath.Join(runDir, "targets.txt")},
		Note:       fmt.Sprintf("%d target(s)", len(names))}, nil
}

func prefixEach(ss []string, p string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = p + s
	}
	return out
}

// buildAPK assembles a debug APK from a source directory using only
// build-tools (aapt2 link + d8 + zipalign + apksigner). This keeps the
// "shared immutable toolchain" story: no Gradle daemon, no per-session
// writable shared state.
//
// Expected source layout (minimal APK):
//
//	src/AndroidManifest.xml
//	src/java/**.java        (optional)
//	src/res/**              (optional)
//	src/assets/**           (optional)
func (a *Adapter) buildAPK(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string, env []string) (*adapter.Result, error) {
	root := sdkRoot(req.Tool)
	if root == "" {
		return nil, fmt.Errorf("android adapter: ANDROID_HOME not set on tool")
	}
	bt := strArg(req.Args, "build_tools", "")
	if bt == "" {
		bt = latestBuildTools(root)
	}
	btdir := filepath.Join(root, "build-tools", bt)
	for _, f := range []string{exeName("aapt2"), batName("d8"), exeName("zipalign"), batName("apksigner")} {
		if _, err := os.Stat(filepath.Join(btdir, f)); err != nil {
			return nil, fmt.Errorf("android adapter: build-tools %s missing %s", bt, f)
		}
	}
	src := strArg(req.Args, "src", "")
	if src == "" {
		src = rt.WorkspaceDir(req.Session.ID)
	}
	if src == "" {
		return nil, fmt.Errorf("android adapter: build.apk requires src (source dir with AndroidManifest.xml)")
	}
	manifest := filepath.Join(src, "AndroidManifest.xml")
	if _, err := os.Stat(manifest); err != nil {
		return nil, fmt.Errorf("android adapter: %s missing AndroidManifest.xml", src)
	}
	pkg := strArg(req.Args, "package", "com.example.arhapk")
	target := strArg(req.Args, "target", latestPlatform(root))
	platformJar := filepath.Join(root, "platforms", target, "android.jar")
	if _, err := os.Stat(platformJar); err != nil {
		return nil, fmt.Errorf("android adapter: no android.jar for target %s", target)
	}
	javaHome := envLookup(env, "JAVA_HOME")
	if javaHome == "" {
		return nil, fmt.Errorf("android adapter: JAVA_HOME required in environment (build tools run java)")
	}

	outDir := filepath.Join(runDir, "apk")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	apkName := strArg(req.Args, "name", "app-debug.apk")
	resDir := filepath.Join(src, "res")
	resArgs := []string{}
	if st, err := os.Stat(resDir); err == nil && st.IsDir() {
		resArgs = []string{"-I", platformJar}
	}
	_ = resArgs

	// 1. aapt2 link -> base APK with resources + manifest
	baseApk := filepath.Join(outDir, "base.apk")
	linkArgs := []string{"link", "-o", baseApk,
		"--manifest", manifest,
		"-I", platformJar,
		"--java", filepath.Join(outDir, "gen"),
	}
	if st, err := os.Stat(resDir); err == nil && st.IsDir() {
		linkArgs = append(linkArgs, "--dir", resDir)
	}
	rt.Log("", "info", fmt.Sprintf("aapt2 link (%s)", pkg))
	if _, err := a.exec(ctx, req, rt, runDir, filepath.Join(btdir, exeName("aapt2")), linkArgs, env, src); err != nil {
		return nil, err
	}

	// 2. Compile any Java sources with javac, then d8 to classes.dex.
	var classFiles []string
	javaSrc := filepath.Join(src, "java")
	genDir := filepath.Join(outDir, "gen")
	classesDir := filepath.Join(outDir, "classes")
	_ = os.MkdirAll(classesDir, 0o755)
	if entries, err := os.ReadDir(javaSrc); err == nil && len(entries) > 0 {
		javac := filepath.Join(javaHome, "bin", exeName("javac"))
		var srcs []string
		err := filepath.Walk(javaSrc, func(p string, info os.FileInfo, err error) error {
			if err == nil && strings.HasSuffix(p, ".java") {
				srcs = append(srcs, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if r := filepath.Join(genDir, pkgPath(pkg), "R.java"); fileExists(r) {
			srcs = append(srcs, r)
		}
		if len(srcs) > 0 {
			rt.Log("", "info", fmt.Sprintf("javac (%d sources)", len(srcs)))
			if _, err := a.exec(ctx, req, rt, runDir, javac,
				append([]string{"-classpath", platformJar, "-d", classesDir}, srcs...), env, src); err != nil {
				return nil, err
			}
			_ = classFiles
		}
		// Collect .class outputs for d8.
		_ = filepath.Walk(classesDir, func(p string, info os.FileInfo, err error) error {
			if err == nil && strings.HasSuffix(p, ".class") {
				classFiles = append(classFiles, p)
			}
			return nil
		})
	}
	dex := filepath.Join(outDir, "classes.dex")
	if len(classFiles) > 0 {
		rt.Log("", "info", fmt.Sprintf("d8 (%d classes)", len(classFiles)))
		d8Args := append([]string{"--lib", platformJar, "--min-api", "24", "--output", outDir}, classFiles...)
		if _, err := a.exec(ctx, req, rt, runDir, filepath.Join(btdir, batName("d8")), d8Args, env, src); err != nil {
			return nil, err
		}
	} else {
		// Empty dex so the APK stays installable.
		if err := os.WriteFile(dex, emptyDex(), 0o644); err != nil {
			return nil, err
		}
	}

	// 3. Package: copy base.apk, add classes.dex, zipalign.
	finalApk := filepath.Join(outDir, apkName)
	if err := copyFile(baseApk, finalApk); err != nil {
		return nil, err
	}
	if err := addToZip(finalApk, "classes.dex", dex); err != nil {
		return nil, err
	}
	aligned := filepath.Join(outDir, "aligned.apk")
	rt.Log("", "info", "zipalign")
	if _, err := a.exec(ctx, req, rt, runDir, filepath.Join(btdir, exeName("zipalign")),
		[]string{"-f", "4", finalApk, aligned}, env, src); err != nil {
		return nil, err
	}

	// 4. Sign with a debug key (generated once per run dir).
	ks := filepath.Join(outDir, "debug.keystore")
	rt.Log("", "info", "apksigner (debug key)")
	if err := makeDebugKeystore(ks, env, javaHome); err != nil {
		return nil, err
	}
	signed := filepath.Join(outDir, "app-debug-signed.apk")
	if err := copyFile(aligned, signed); err != nil {
		return nil, err
	}
	if _, err := a.exec(ctx, req, rt, runDir, filepath.Join(btdir, batName("apksigner")),
		[]string{"sign", "--ks", ks, "--ks-pass", "pass:android", "--ks-key-alias", "androiddebugkey",
			"--key-pass", "pass:android", "--out", signed, signed}, env, src); err != nil {
		return nil, err
	}
	// apksigner with --out and the same input/output path can truncate on
	// some versions; if the signed file is missing/empty fall back to
	// in-place signing of a fresh copy.
	if fi, err := os.Stat(signed); err != nil || fi.Size() == 0 {
		if err := copyFile(aligned, signed); err != nil {
			return nil, err
		}
		if _, err := a.exec(ctx, req, rt, runDir, filepath.Join(btdir, batName("apksigner")),
			[]string{"sign", "--ks", ks, "--ks-pass", "pass:android", "--ks-key-alias", "androiddebugkey",
				"--key-pass", "pass:android", signed}, env, src); err != nil {
			return nil, err
		}
	}

	code := 0
	return &adapter.Result{
		ExitCode: &code, Status: model.RunSucceeded,
		ExtraFiles: map[string]string{"apk": signed},
		Note:       fmt.Sprintf("signed %s (%s)", signed, humanSize(signed)),
	}, nil
}

// deviceCommand runs install/shell/screenshot/uninstall against the leased serial.
func (a *Adapter) deviceCommand(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string, env []string) (*adapter.Result, error) {
	adb, err := adbPath(req.Tool)
	if err != nil {
		return nil, err
	}
	serial := strArg(req.Args, "serial", strArg(req.Args, "device", ""))
	dir := rt.WorkspaceDir(req.Session.ID)
	var args []string
	switch req.Command {
	case "install":
		apk := strArg(req.Args, "apk", "")
		if apk == "" {
			return nil, fmt.Errorf("android adapter: install requires apk")
		}
		if !filepath.IsAbs(apk) && dir != "" {
			apk = filepath.Join(dir, apk)
		}
		args = []string{"-s", serial, "install", "-r", "-t", apk}
	case "uninstall":
		pkg := strArg(req.Args, "package", "")
		if pkg == "" {
			return nil, fmt.Errorf("android adapter: uninstall requires package")
		}
		args = []string{"-s", serial, "uninstall", pkg}
	case "shell":
		cmd := strArg(req.Args, "cmd", "")
		if cmd == "" {
			return nil, fmt.Errorf("android adapter: shell requires cmd")
		}
		args = []string{"-s", serial, "shell", cmd}
	case "screenshot":
		args = []string{"-s", serial, "exec-out", "screencap -p"}
	}
	rt.Log("", "info", fmt.Sprintf("adb %s (serial %s)", req.Command, serial))
	res, err := a.exec(ctx, req, rt, runDir, adb, args, env, dir)
	if err != nil {
		return nil, err
	}
	// Collect the screenshot as a run artifact.
	if req.Command == "screenshot" && res.ExitCode != nil && *res.ExitCode == 0 {
		dst := filepath.Join(runDir, "screenshot.png")
		if err := copyRunLog(filepath.Join(runDir, "stdout.log"), dst); err == nil {
			if res.ExtraFiles == nil {
				res.ExtraFiles = map[string]string{}
			}
			res.ExtraFiles["screenshot"] = dst
		}
	}
	return res, nil
}

// createAVD creates an AVD inside the session's isolated ANDROID_AVD_HOME
// (falls back to a run-dir avd home). The AVD is session-scoped: it never
// touches the host's ~/.android/avd.
func (a *Adapter) createAVD(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string, env []string) (*adapter.Result, error) {
	root := sdkRoot(req.Tool)
	if root == "" {
		return nil, fmt.Errorf("android adapter: ANDROID_HOME not set on tool")
	}
	name := strArg(req.Args, "name", "")
	if name == "" {
		return nil, fmt.Errorf("android adapter: avd.create requires name")
	}
	pkgID := strArg(req.Args, "package", "")
	if pkgID == "" {
		// First installed system image wins.
		pkgID = firstSystemImage(root)
		if pkgID == "" {
			return nil, fmt.Errorf("android adapter: no system-images installed; pass package=")
		}
	}
	// The AVD home is session-scoped: <session runtime dir>/avd. It must
	// outlive any single run (create -> boot are different runs) and must
	// never touch the host's ~/.android/avd.
	avdHome := sessionAVDHome(req.Session)
	if err := os.MkdirAll(avdHome, 0o755); err != nil {
		return nil, err
	}
	if envLookup(env, "ANDROID_AVD_HOME") == "" {
		env = append(env, "ANDROID_AVD_HOME="+avdHome)
	}
	avdmanager := filepath.Join(root, "cmdline-tools", "latest", "bin", "avdmanager.bat")
	if filepath.Separator != '\\' {
		avdmanager = filepath.Join(root, "cmdline-tools", "latest", "bin", "avdmanager")
	}
	if _, err := os.Stat(avdmanager); err != nil {
		return nil, fmt.Errorf("android adapter: avdmanager not found at %s", avdmanager)
	}
	rt.Log("", "info", fmt.Sprintf("avdmanager create %s (%s)", name, pkgID))
	res, err := a.exec(ctx, req, rt, runDir, avdmanager,
		[]string{"create", "avd", "-n", name, "-k", pkgID, "-d", "pixel_6"},
		env, rt.WorkspaceDir(req.Session.ID))
	if err != nil {
		return nil, err
	}
	// Record where the AVD landed for the boot command.
	_ = os.WriteFile(filepath.Join(runDir, "avd-home.txt"), []byte(avdHome), 0o644)
	return res, nil
}

// bootEmulator launches an AVD headless under supervision and waits for
// the system to reach the "device" state. The emulator process is a normal
// supervised child: it STAYS RUNNING after this run succeeds (it is a
// service, not a batch job); session termination or crash recovery kills
// it with the rest of the session's processes.
func (a *Adapter) bootEmulator(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string, env []string) (*adapter.Result, error) {
	emu, err := emulatorPath(req.Tool)
	if err != nil {
		return nil, err
	}
	name := strArg(req.Args, "name", "")
	if name == "" {
		return nil, fmt.Errorf("android adapter: emulator.boot requires name (AVD)")
	}
	serial := strArg(req.Args, "serial", "emulator-5554")
	port := strings.TrimPrefix(serial, "emulator-")
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("android adapter: serial must look like emulator-5554, got %q", serial)
	}
	avdHome := sessionAVDHome(req.Session)
	if err := os.MkdirAll(avdHome, 0o755); err != nil {
		return nil, err
	}
	if envLookup(env, "ANDROID_AVD_HOME") == "" {
		env = append(env, "ANDROID_AVD_HOME="+avdHome)
	}
	args := []string{"-avd", name,
		"-port", port,
		"-no-window", "-no-audio", "-no-boot-anim",
		"-no-snapshot", "-gpu", "swiftshader_indirect",
	}
	rt.Log("", "info", fmt.Sprintf("emulator boot %s (serial %s)", name, serial))

	// Fire-and-forget supervised launch: the process outlives this run.
	// The Runtime bundle exposes Exec (which waits); for a service we need
	// a non-waiting launch, so we drive the supervisor through a command
	// marker: launch via a shell that pauses forever would hold the slot;
	// instead we ask the daemon Runtime for a detached launch.
	handle, err := rt.LaunchDetached(ctx, adapter.ExecRequest{
		SessionID: req.Session.ID,
		Label:     fmt.Sprintf("android:emulator(%s)", name),
		Exe:       emu,
		Args:      args,
		Env:       env,
		Dir:       runDir,
		RunDir:    runDir,
		Timeout:   0, // no timeout: service lifecycle is the session's
	})
	if err != nil {
		return nil, err
	}
	// Poll for system readiness through adb.
	adb, err := adbPath(req.Tool)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(a.timeoutFor("emulator.boot"))
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
		out, err := exec.CommandContext(ctx, adb, "-s", serial, "get-state").CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) == "device" {
			code := 0
			return &adapter.Result{
				ExitCode: &code, Status: model.RunSucceeded,
				Note:       fmt.Sprintf("%s booted (pid %d)", serial, handle.PID()),
				ExtraFiles: map[string]string{"emulator-log": filepath.Join(runDir, "stdout.log")},
			}, nil
		}
		// Process died before booting?
		if !handle.Alive() {
			return nil, fmt.Errorf("emulator exited before boot (see run stdout.log)")
		}
	}
	return nil, fmt.Errorf("emulator %s did not reach device state within %s", serial, a.timeoutFor("emulator.boot"))
}

// sessionAVDHome returns the session-scoped AVD home. AVDs live with the
// session lifecycle (create once, boot/install/screenshot in later runs);
// session termination removes them with the runtime dir.
func sessionAVDHome(sess *model.Session) string {
	return filepath.Join(sess.RuntimeDir, "avd")
}

// emulatorPath resolves the emulator binary.
func emulatorPath(tool *model.Tool) (string, error) {
	if v, ok := tool.Env["ARH_EMULATOR"]; ok && v != "" {
		return v, nil
	}
	root := sdkRoot(tool)
	if root == "" {
		return "", fmt.Errorf("android adapter: ANDROID_HOME not set on tool")
	}
	emu := filepath.Join(root, "emulator", exeName("emulator"))
	if _, err := os.Stat(emu); err != nil {
		return "", fmt.Errorf("android adapter: emulator not found at %s", emu)
	}
	return emu, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func strArg(args map[string]any, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return def
}

func envLookup(env []string, name string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return kv[len(name)+1:]
		}
	}
	return ""
}

// latestBuildTools picks the highest installed build-tools version.
func latestBuildTools(root string) string {
	entries, err := os.ReadDir(filepath.Join(root, "build-tools"))
	if err != nil {
		return ""
	}
	var best string
	var bestN [4]int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if n, ok := parseVer4(e.Name()); ok {
			if less(bestN, n) {
				bestN = n
				best = e.Name()
			}
		}
	}
	return best
}

// latestPlatform picks the highest android-XX platform.
func latestPlatform(root string) string {
	entries, err := os.ReadDir(filepath.Join(root, "platforms"))
	if err != nil {
		return ""
	}
	best := ""
	bestN := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "android-") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "android-")); err == nil && n > bestN {
			bestN = n
			best = e.Name()
		}
	}
	return best
}

func parseVer4(s string) ([4]int, bool) {
	var n [4]int
	parts := strings.Split(s, ".")
	if len(parts) == 0 {
		return n, false
	}
	for i, p := range parts {
		if i >= 4 {
			break
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return n, false
		}
		n[i] = v
	}
	return n, true
}

func less(a, b [4]int) bool {
	for i := 0; i < 4; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// firstSystemImage finds an installed system-image package id.
// On-disk layout: system-images/<api>/<tag>/<abi>/ (e.g.
// system-images/android-36.1/google_apis_playstore/x86_64); the package id
// keeps that order: system-images;<api>;<tag>;<abi>.
func firstSystemImage(root string) string {
	base := filepath.Join(root, "system-images")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, api := range entries {
		if !api.IsDir() {
			continue
		}
		tags, err := os.ReadDir(filepath.Join(base, api.Name()))
		if err != nil {
			continue
		}
		for _, tag := range tags {
			if !tag.IsDir() {
				continue
			}
			abis, err := os.ReadDir(filepath.Join(base, api.Name(), tag.Name()))
			if err != nil {
				continue
			}
			for _, abi := range abis {
				if abi.IsDir() {
					return fmt.Sprintf("system-images;%s;%s;%s", api.Name(), tag.Name(), abi.Name())
				}
			}
		}
	}
	return ""
}

func pkgPath(pkg string) string { return strings.ReplaceAll(pkg, ".", "/") }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// copyRunLog moves/copies a run log (stdout.log) to dst (screenshot path).
func copyRunLog(src, dst string) error { return copyFile(src, dst) }

// addToZip appends a file entry to a ZIP (APK) archive in place.
func addToZip(apk, entryName, srcPath string) error {
	// Read the existing archive.
	r, err := os.Open(apk)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	// Rewrite: copy all entries + the new one.
	tmp := apk + ".tmp"
	w, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			w.Close()
			os.Remove(tmp)
			return err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			w.Close()
			os.Remove(tmp)
			return err
		}
		hdr := f.FileHeader
		// Keep compression off for .dex (uncompressed dex required by
		// modern Android for shared libraries; harmless otherwise).
		if strings.HasSuffix(hdr.Name, ".dex") {
			hdr.Method = zip.Store
		}
		out, err := zw.CreateHeader(&hdr)
		if err != nil {
			w.Close()
			os.Remove(tmp)
			return err
		}
		if _, err := out.Write(b); err != nil {
			w.Close()
			os.Remove(tmp)
			return err
		}
	}
	dexData, err := os.ReadFile(srcPath)
	if err != nil {
		w.Close()
		os.Remove(tmp)
		return err
	}
	hdr := zip.FileHeader{Name: "classes.dex", Method: zip.Store}
	out, err := zw.CreateHeader(&hdr)
	if err != nil {
		w.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := out.Write(dexData); err != nil {
		w.Close()
		os.Remove(tmp)
		return err
	}
	if err := zw.Close(); err != nil {
		w.Close()
		os.Remove(tmp)
		return err
	}
	if err := w.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, apk)
}

// emptyDex is a minimal valid dex (header only) so an APK without classes
// still installs.
func emptyDex() []byte {
	d := make([]byte, 112)
	copy(d[0:], "dex\n035\000")
	binary.LittleEndian.PutUint32(d[32:], 112) // file size
	binary.LittleEndian.PutUint32(d[40:], 112) // data size... keep header-only
	binary.LittleEndian.PutUint32(d[88:], 1)   // header size
	return d
}

// makeDebugKeystore generates a fresh debug keystore with keytool.
func makeDebugKeystore(path string, env []string, javaHome string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	keytool := filepath.Join(javaHome, "bin", exeName("keytool")) // keytool is exe on windows, script elsewhere
	dname := "CN=Android Debug,O=Android,C=US"
	cmd := exec.Command(keytool, "-genkeypair",
		"-keystore", path,
		"-storepass", "android", "-keypass", "android",
		"-alias", "androiddebugkey", "-keyalg", "RSA", "-keysize", "2048",
		"-validity", "10000", "-dname", dname)
	cmd.Env = append(env, "JAVA_HOME="+javaHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("keytool: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func humanSize(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	return fmt.Sprintf("%d bytes", fi.Size())
}

var _ adapter.ToolAdapter = (*Adapter)(nil)
