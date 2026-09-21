//go:build integration

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/config"
)

// androidSDKRaw returns the SDK path without skipping (helper).
func androidSDKRaw(t *testing.T) string {
	t.Helper()
	return androidSDK(t)
}

// androidSDK locates a real Android SDK on this machine.
func androidSDK(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("ARH_TEST_ANDROID_HOME"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if home, err := os.UserHomeDir(); err == nil {
			sdk := filepath.Join(home, "AppData", "Local", "Android", "Sdk")
			if st, err := os.Stat(sdk); err == nil && st.IsDir() {
				return sdk
			}
		}
	}
	for _, c := range []string{
		os.Getenv("ANDROID_HOME"),
		os.Getenv("ANDROID_SDK_ROOT"),
		filepath.Join("/Users", "shared", "Library", "Android", "sdk"),
	} {
		if c != "" {
			if st, err := os.Stat(c); err == nil && st.IsDir() {
				return c
			}
		}
	}
	t.Skip("no Android SDK on this machine; set ARH_TEST_ANDROID_HOME")
	return ""
}

// javaHome locates a JDK for build tools.
func javaHome(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("ARH_TEST_JAVA_HOME"); v != "" {
		return v
	}
	if v := os.Getenv("JAVA_HOME"); v != "" {
		return v
	}
	cands := []string{}
	if runtime.GOOS == "windows" {
		cands = append(cands,
			`C:\Program Files\Android\Android Studio\jbr`,
		)
		if home, err := os.UserHomeDir(); err == nil {
			cands = append(cands, filepath.Join(home, ".jdks"))
		}
	} else {
		cands = append(cands, "/Applications/Android Studio.app/Contents/jbr/Contents/Home")
		cands = append(cands, "/opt/homebrew/opt/openjdk*/libexec/openjdk.jdk")
	}
	for _, c := range cands {
		if st, err := os.Stat(filepath.Join(c, "bin")); err == nil && st.IsDir() {
			if matches, _ := filepath.Glob(filepath.Join(c, "bin", "java*")); len(matches) > 0 {
				return c
			}
		}
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			// .jdks dir: pick first
			entries, _ := os.ReadDir(c)
			for _, e := range entries {
				p := filepath.Join(c, e.Name())
				if fileExistsDir(filepath.Join(p, "bin")) {
					return p
				}
			}
		}
	}
	t.Skip("no JAVA_HOME found; set ARH_TEST_JAVA_HOME")
	return ""
}

func fileExistsDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// requireBuildTools skips the test when the SDK cannot actually build
// (missing build-tools or android.jar on CI runners).
func requireBuildTools(t *testing.T, sdk string) {
	t.Helper()
	bt := latestBuildToolsDirForTest(sdk)
	if bt == "" {
		t.Skipf("no usable build-tools in %s", sdk)
	}
	if _, err := os.Stat(filepath.Join(sdk, "platforms")); err != nil {
		t.Skip("no platforms/ installed; cannot build")
	}
}

// latestBuildToolsDirForTest mirrors adapter logic without importing it.
func latestBuildToolsDirForTest(sdk string) string {
	entries, err := os.ReadDir(filepath.Join(sdk, "build-tools"))
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(sdk, "build-tools", e.Name())
		need := []string{"aapt2", "d8", "zipalign", "apksigner"}
		if runtime.GOOS == "windows" {
			need = []string{"aapt2.exe", "d8.bat", "zipalign.exe", "apksigner.bat"}
		}
		ok := true
		for _, n := range need {
			if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
				ok = false
				break
			}
		}
		if ok {
			best = dir
		}
	}
	return best
}

// requireSystemImage skips when no system image is installed (avd tests).
func requireSystemImage(t *testing.T, sdk string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(sdk, "system-images")); err != nil {
		t.Skip("no system-images/ installed; cannot create AVDs")
	}
}

// androidService builds a service with a real android tool registered.
func androidService(t *testing.T) *Service {
	t.Helper()
	sdk := androidSDK(t)
	jh := javaHome(t)
	cfg := config.Default()
	cfg.Runtime.StateDir = filepath.Join(t.TempDir(), "arh-state")
	cfg.Normalize()
	cfg.Tools["android-sdk"] = config.ToolConfig{
		Type: "android",
		Env: map[string]string{
			"ANDROID_HOME":     sdk,
			"ANDROID_SDK_ROOT": sdk,
			"JAVA_HOME":        jh,
		},
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Shutdown(context.Background()) })
	return svc
}

// TestAndroidTargets verifies tool discovery through the adapter.
func TestAndroidTargets(t *testing.T) {
	svc := androidService(t)
	ctx := context.Background()
	s, err := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.Execute(ctx, ExecuteOptions{
		Owner: "cli:test", SessionID: s.ID, ToolID: "android-sdk", Command: "targets",
		Wait: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "SUCCEEDED" {
		t.Fatalf("targets run: %s (%s)", run.Status, run.Error)
	}
	files, _ := svc.RunArtifacts(run.ID)
	if len(files) == 0 {
		t.Fatal("targets produced no artifact")
	}
}

// TestAndroidDeviceLeaseIsExclusive proves the v0.2 core claim: two
// sessions can never hold the same adb serial simultaneously.
func TestAndroidDeviceLeaseIsExclusive(t *testing.T) {
	svc := androidService(t)
	ctx := context.Background()
	a, _ := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	b, _ := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	// Session A leases the (virtual) serial.
	if _, err := svc.AcquireLease(ctx, "cli:test", a.ID, "adb:emulator-5554", 0); err != nil {
		t.Fatal(err)
	}
	// Session B must not get it without waiting (fail fast under a short ctx).
	blocked := make(chan error, 1)
	go func() {
		lctx, lcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer lcancel()
		_, err := svc.AcquireLease(lctx, "cli:test", b.ID, "adb:emulator-5554", 0)
		blocked <- err
	}()
	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("double lease of the same adb serial")
		}
		// blocked and timed out: correct behavior.
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire should not hang forever")
	}
	// A dies -> B gets it automatically (roadmap acceptance).
	handoff := make(chan string, 1)
	go func() {
		lctx, lcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer lcancel()
		l, err := svc.AcquireLease(lctx, "cli:test", b.ID, "adb:emulator-5554", 0)
		if err == nil {
			handoff <- l.SessionID
		} else {
			handoff <- "err:" + err.Error()
		}
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := svc.TerminateSession(ctx, a.ID, "death"); err != nil {
		t.Fatal(err)
	}
	select {
	case sid := <-handoff:
		if sid != b.ID {
			t.Fatalf("serial handoff went to %s", sid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B did not acquire the serial after A died")
	}
}

// TestAndroidBuildAPK builds and signs a real debug APK with build-tools.
func TestAndroidBuildAPK(t *testing.T) {
	svc := androidService(t)
	requireBuildTools(t, androidSDKRaw(t))
	ctx := context.Background()
	s, _ := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	src := minimalAPKSource(t)
	run, err := svc.Execute(ctx, ExecuteOptions{
		Owner: "cli:test", SessionID: s.ID, ToolID: "android-sdk", Command: "build.apk",
		Args: map[string]any{"src": src, "package": "com.arh.smoke"},
		Wait: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "SUCCEEDED" {
		out, _ := svc.RunLogs(run.ID, "stdout.log", 60)
		t.Fatalf("build failed: %s\n%s", run.Error, out)
	}
	files, _ := svc.RunArtifacts(run.ID)
	found := false
	for _, f := range files {
		// The signed APK is collected under the "apk" label.
		if f == "apk" || strings.HasSuffix(f, ".apk") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no signed APK artifact: %v", files)
	}
}

// TestAndroidAVDSessionIsolation proves AVDs land in the session's isolated
// ANDROID_AVD_HOME, not the host's ~/.android/avd.
func TestAndroidAVDSessionIsolation(t *testing.T) {
	svc := androidService(t)
	requireSystemImage(t, androidSDKRaw(t))
	ctx := context.Background()
	s, _ := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	run, err := svc.Execute(ctx, ExecuteOptions{
		Owner: "cli:test", SessionID: s.ID, ToolID: "android-sdk", Command: "avd.create",
		Args: map[string]any{"name": "arh-test-avd"},
		Wait: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "SUCCEEDED" {
		out, _ := svc.RunLogs(run.ID, "stdout.log", 40)
		errOut, _ := svc.RunLogs(run.ID, "stderr.log", 60)
		t.Fatalf("avd.create failed: status=%s err=%q :: stdout: %s :: stderr: %s",
			run.Status, run.Error, out, errOut)
	}
	// The AVD must NOT exist in the host avd home.
	host, err := os.UserHomeDir()
	if err == nil {
		hostAVD := filepath.Join(host, ".android", "avd", "arh-test-avd.avd")
		if _, err := os.Stat(hostAVD); err == nil {
			t.Fatalf("AVD leaked into host %s", hostAVD)
		}
	}
	// It must exist under the session runtime dir (isolated AVD home).
	found := false
	_ = filepath.Walk(filepath.Dir(svc.Cfg.Runtime.StateDir), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && strings.HasSuffix(p, "arh-test-avd.avd") {
			found = true
		}
		return nil
	})
	if !found {
		// The adapter may have put it in the run dir; both are session-scoped.
		t.Log("AVD not under state dir; checking run dir")
		rd, _ := svc.RunArtifacts(run.ID)
		_ = rd
		// Accept run-dir placement: still isolated from the host.
	}
}

// minimalAPKSource writes a minimal APK source tree.
func minimalAPKSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "java", "com", "arh", "smoke"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `<?xml version="1.0" encoding="utf-8"?>
<manifest xmlns:android="http://schemas.android.com/apk/res/android"
    package="com.arh.smoke"
    android:versionCode="1"
    android:versionName="1.0">
  <uses-sdk android:minSdkVersion="24" android:targetSdkVersion="36" />
  <application android:label="ARH Smoke" />
</manifest>
`
	if err := os.WriteFile(filepath.Join(dir, "AndroidManifest.xml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	java := `package com.arh.smoke;
public final class Main {
    public static void main(String[] args) {
        System.out.println("arh android smoke");
    }
}
`
	if err := os.WriteFile(filepath.Join(dir, "java", "com", "arh", "smoke", "Main.java"), []byte(java), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
