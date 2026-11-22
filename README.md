# Java and Kotlin for CED

CED identifies and highlights `.java`, `.kt` and `.kts` buffers. This extension provides Eclipse JDT LS for Java and JetBrains Kotlin LSP for Kotlin. After Maven or Gradle imports the project, the editor can show diagnostics, completion and hover information, and jump to source definitions.

## Build and install

Run `make package` with Go 1.23+ and Make, then choose **Install Extension from Folder** in a CED build with the JVM language-server contribution. Select `dist/viniciusfonseca.java-kotlin/` and approve the requested `documents.read` and `documents.edit` grants for the editing commands.

Install the servers you want to use. The launcher looks for `jdtls` for Java and `kotlin-lsp` or `intellij-server` for Kotlin on the inherited PATH and in standard Homebrew and local bin directories. You can set `CED_JDTLS` or `CED_KOTLIN_LSP` to the absolute executable path before starting CED. Set `JAVA_HOME` to a JDK supported by the JDT LS build you install. The validation used JDK 25 to run JDT LS 1.64.0 and JDK 21 to build the examples. Kotlin Maven import uses the project's Maven wrapper or `mvn` on PATH. Gradle projects should include the Gradle wrapper. An optional absolute `CED_LSP_LOG` path collects the servers' standard error.

- [Eclipse JDT LS](https://github.com/eclipse-jdtls/eclipse.jdt.ls)
- [JetBrains Kotlin LSP standalone releases](https://github.com/Kotlin/kotlin-lsp/releases)

The package builds for macOS arm64 and x86_64 and Linux arm64 and x86_64. See [TESTING.md](TESTING.md) for the platforms and server versions exercised. The two text-editing commands work without a JVM or language server.

## Import a project

1. Open the project folder in CED and choose **Trust Workspace** in **Manage Extensions**.
2. Open a saved Java or Kotlin file. CED looks for `pom.xml`, `settings.gradle[.kts]` or `build.gradle[.kts]` in the file's ancestors within the workspace and starts the matching server. The status bar shows the language, build tool and import status.
3. Use **Go to Definition** (F12), **Symbol Information**, or **Complete** from the palette. Use **Import JVM Project** with a source file open to reload the project's settings and model.

To point CED at a specific Maven project, put this in the workspace's `.ced/settings.json`:

```json
{
  "maven.pomPath": "modules/service/pom.xml",
  "maven.settingsPath": "~/.m2/settings-work.xml"
}
```

For Gradle, set `"gradle.projectPath": "modules/service"` instead. Use one nonempty project-root override. Set `"gradle.javaHome": "/absolute/path/to/jdk"` if the Gradle wrapper needs a different JDK from the language server. Project-root paths are relative to the open workspace or absolute within it. The source file must be under the selected root. Workspace values override the corresponding values in `~/.ced/settings.json`. An empty string clears an inherited override. Save the settings and run **Import JVM Project** if the source tab is already open.

Use **Disable** under **Manage Extensions** to stop this extension's language and editing services. CED supplies lexical highlighting directly. Definitions with ordinary file URIs can be opened in CED. Server-specific virtual documents such as JDT class-file URIs require a separate viewer.

`maven.settingsPath` selects a Maven user settings file. Use an absolute path, a path beginning with `~/`, or a path relative to the open workspace that resolves inside it. Leaving the value empty uses the Maven defaults. JDT LS receives the file as its Maven user settings. For Kotlin, use Maven 3.9 or later in the Maven executable or wrapper used by Kotlin LSP, so that Maven accepts the settings argument through `MAVEN_ARGS`.

## Editing commands

| Palette title | Command ID | Behavior |
| --- | --- | --- |
| **Sort Java/Kotlin Imports** | `viniciusfonseca.java-kotlin.sortImports` | Sorts contiguous groups of single-line header imports by qualified path, case-sensitively. Keeps duplicates, wildcard imports, Kotlin aliases, spacing, blank lines and line endings. Sorts Java static and ordinary imports in separate groups. Comments delimit groups and the first import immediately below a comment stays in place. |
| **Insert Java/Kotlin Main** | `viniciusfonseca.java-kotlin.insertMain` | Inserts a Java class with `public static void main(String[] args)`, a Kotlin top-level `fun main()`, or a Kotlin script that calls `main()`. Requires a saved, empty buffer. Java class names come from the filename and use ASCII letters, digits and underscores. The template has no package declaration. |

Both commands take JSON `null` arguments and return `{"changed":boolean,"version":positive integer}`. Invalid input returns `-32602`. A successful no-op returns the current version. Commands use unsaved text and each change is one undoable transaction. A stale buffer or selection cancels the edit. For the sorter, unsupported header syntax such as file annotations, multiline imports, inline import comments and non-ASCII qualified names leaves the file unchanged.

## CED API compatibility

Version 0.2.1 targets CED 0.2.0 development **with the JVM language-server host contribution**, manifest format 1 and experimental extension API 1.0. The editing peer requires `documents/get` and `documents/applyEdits`. The server launcher uses the separate `--language-server <java|kotlin> <absolute-project-root> [<gradle-jdk>]` entrypoint and streams LSP. The installed package remains read-only. A compatible CED build is required for this manifest.
