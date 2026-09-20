# Unity example

Real Unity integration against an installed editor.

## 0. Prereqs

- Unity Hub with an editor installed (6000.x recommended)
- a git-tracked Unity project

## 1. Configure

`~/.agent-runtime/config.yaml`:

```yaml
tools:
  # Either pin the editor explicitly...
  unity-6000:
    type: unity
    version: "6000.3"
    executable: /Applications/Unity/Hub/Editor/6000.3/Unity.app/Contents/MacOS/Unity
  # ...or rely on Hub auto-detection + default_version:
  # unity:
  #   default_version: ">= 6000.0"

resources:
  gpu:              { mode: exclusive }
  unity-license:    { mode: exclusive }
  unity-process-slot: { mode: capacity, capacity: 2 }

unity:
  cache:
    accelerator_enabled: true
    accelerator_host: 127.0.0.1
```

## 2. Validate the install

```bash
arh daemon start -d
arh tool list
arh tool doctor unity-6000
arh tool resolve "unity >= 6000.0"
```

## 3. Workspace + session

```bash
arh workspace create --repo ~/my-game --branch agent/payment-fix
arh session create --workspace payment-fix
# => sess-000001
```

## 4. Run EditMode tests

```bash
arh exec "unity >= 6000.0".test.editmode --session sess-000001 --timeout-min 30
arh run list --session sess-000001
arh logs run-000001 --name unity.log --tail 50
arh artifacts run-000001
# => test-results  (parsed NUnit3 summary in run metadata)
```

## 5. Build a player

Your project needs a build entry method, e.g.
`Assets/Editor/CIScript.cs`:

```csharp
public static class CIScript {
    public static void BuildPlayer() {
        var scenes = EditorBuildSettings.scenes
            .Where(s => s.enabled).Select(s => s.path).ToArray();
        BuildPipeline.BuildPlayer(scenes,
            "../Builds/Android.apk",
            BuildTarget.Android, BuildOptions.None);
    }
}
```

```bash
arh exec "unity >= 6000.0".build --session sess-000001 -- \
  target=Android method=CIScript.BuildPlayer
```

## 6. Batch method

```bash
arh exec "unity >= 6000.0".run.batch --session sess-000001 -- \
  method=MyNamespace.MyStaticMethod
```

## 7. Through MCP (agent-driven)

```jsonc
// tools/call
{"name": "unity_run_tests",
 "arguments": {"session": "sess-000001", "mode": "editmode"}}
```

## Notes

- `-forgetProjectPath` is passed on test runs to avoid project-path cache
  pollution across batch invocations.
- the editor log lands at `runs/<id>/unity.log`; test XML at
  `runs/<id>/test-results.xml`, collected into `artifacts/test-results`.
- `Library/` stays per-workspace (worktree); enable the Accelerator to share
  import artifacts safely — see `docs/unity-cache.md`.
