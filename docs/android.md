# Android Adapter (v0.2)

The second real toolchain (roadmap §26): proving the abstraction is not
Unity-specific. Same daemon, same lease manager, same supervisor — only a
new adapter.

## What it covers

| Roadmap v0.2 requirement | Implementation |
|---|---|
| simulator/device leases | `adb:<serial>` dynamic exclusive resources, registered on demand; `android-emulator-slot` capacity resource |
| tool-specific resource discovery | `targets` / `devices` / `emulators` commands read the real SDK + adb state |
| parallel test jobs | exclusive serial leases serialize per-device work across sessions; capacity slots bound concurrent emulators |
| artifact capture | signed APKs, screenshots, target lists, emulator logs collected under `runs/<id>/artifacts/` |

## Commands

```text
devices        list adb devices (no lease)
emulators      list AVDs visible to the session's isolated AVD home
targets        list installed SDK platforms (artifact: targets.txt)
build.apk      aapt2 link + javac + d8 + zipalign + apksigner (signed APK artifact)
avd.create     create an AVD under <session runtime dir>/avd (never ~/.android)
emulator.boot  supervised service launch; waits for the device state
install        install an APK onto a leased serial (needs adb:<serial>)
shell          run a shell command on a leased serial (needs adb:<serial>)
screenshot     screencap a leased serial (artifact: screenshot.png)
uninstall      remove a package from a leased serial (needs adb:<serial>)
```

## Resource model

Device commands (`install`, `shell`, `screenshot`, `uninstall`) **require a
serial** and lease `adb:<serial>` exclusively for the run:

```text
Session A ── lease adb:emulator-5554 ──► holds it exclusively
Session B ── shell on emulator-5554 ──► queued (FIFO) until A's run ends
Session A dies mid-run ────────────────► B is granted automatically
```

Two sessions can therefore never touch the same device concurrently — the
core v0.2 claim, covered by `TestAndroidDeviceLeaseIsExclusive`.

`emulator.boot` holds an `android-emulator-slot` (capacity, default 2) so N
concurrent emulator boots are bounded.

## Configuration

```yaml
tools:
  android-sdk:
    type: android            # no executable needed: binaries derive from ANDROID_HOME
    env:
      ANDROID_HOME: C:/Users/me/AppData/Local/Android/Sdk
      ANDROID_SDK_ROOT: C:/Users/me/AppData/Local/Android/Sdk
      JAVA_HOME: C:/Program Files/Android/Android Studio/jbr
```

`JAVA_HOME` is required for build-tool wrappers (d8/apksigner/keytool).

## Isolation model

- **AVDs live in `<session runtime dir>/avd`** (`ANDROID_AVD_HOME`), never
  the host's `~/.android/avd`. Concurrent sessions never race over AVD
  `.ini` files; session teardown removes them with the runtime dir.
- The AVD home is **session-scoped, not run-scoped**: `avd.create` and
  `emulator.boot` are different runs, so the home must outlive any run.
- `emulator.boot` is a **service-style supervised launch**
  (`Runtime.LaunchDetached`): the emulator keeps running after the run
  succeeds, and dies with the session (or is reaped by crash recovery).

## Build pipeline (no Gradle)

`build.apk` deliberately uses only build-tools — no Gradle daemon, no
per-session writable shared state:

```text
aapt2 link  ──► base.apk (resources + manifest + gen/R.java)
javac       ──► classes (platform android.jar on classpath)
d8          ──► classes.dex
zip add     ──► classes.dex into the APK (stored, uncompressed)
zipalign    ──► aligned.apk
keytool     ──► fresh debug keystore per run dir
apksigner   ──► app-debug-signed.apk  (artifact: "apk")
```

## CLI

```bash
arh exec android-sdk.devices --session sess-1
arh exec android-sdk.targets --session sess-1
arh exec android-sdk.build.apk --session sess-1 -- src=/path/to/project package=com.example.app
arh exec android-sdk.avd.create --session sess-1 -- name=test-avd
arh exec android-sdk.emulator.boot --session sess-1 -- name=test-avd serial=emulator-5574
arh exec android-sdk.install --session sess-1 -- serial=emulator-5574 apk=app-debug-signed.apk
arh exec android-sdk.screenshot --session sess-1 -- serial=emulator-5574
```

## Notes & limits (v0.2)

- A full first boot of a Play-store system image can take a long time on
  software graphics; `emulator.boot` waits up to its timeout (default 10m,
  override with `--timeout-min`) for the `device` state.
- Physical devices appear as `adb:<serial>` resources the same way — the
  lease model does not distinguish emulators from real hardware.
- `install`/`shell`/`screenshot`/`uninstall` fail fast in `Prepare` when
  `serial`/`device` is missing, before any process is spawned.
