# Test report

## Desktop validation on macOS arm64

Separate CED windows used isolated validation profiles. The editor was built from CED 0.2.0 development, based on `797266a` plus the JVM changes. Real servers were Eclipse JDT LS 1.64.0 and JetBrains Kotlin LSP 271.2085.0.

- **Java and Maven.** Built the Spring Boot sample with Maven 3.9.12 and JDK 21. In CED, checked the `java` language indicator and token colors, Maven ready status, hover documentation for Spring's `@GetMapping`, and F12 from the `Greeting` reference to its declaration. The workspace provided `maven.pomPath` in `.ced/settings.json`.
- **Kotlin and Maven.** Built the Spring Boot sample with the same Maven and JDK. In CED, checked Kotlin highlighting, Maven ready status, Spring's `@GetMapping` hover, and F12 to the `Greeting` data class. The project root was detected automatically.

Two independent Gradle 8.14.3 projects also built successfully. With JDK 21 selected through `gradle.javaHome`, the Java and Kotlin windows both showed Gradle ready. F12 opened `Greeting.java` from `App.java`, and `Greeting.kt` from `App.kt`. A direct LSP check independently verified both definition URIs. The JDT LS process used Java 25.

## Maven user settings

CED and the packaged extension imported both Spring Boot samples through real servers using a workspace-relative XML path with spaces. JDT LS applied the configured local repository and mirror, verified by a second import against an empty file-based mirror. Kotlin LSP with Maven 3.9.12 applied the configured local repository and offline setting, verified by rejecting a fresh import against an empty cache while offline. The package test also checks validation of relative, absolute, missing, directory and empty settings paths.

## Automated checks

- All 13 CED CTest suites passed on macOS arm64, including the clangd regression suite. An extensions-disabled build also compiled.
- `make test`, `make test-ced` and `go vet ./...` verify the package, protocol, editor commands, language-server launcher, native highlighting, workspace settings and provider trust. The mock tests launch the real packaged executable.
- The packaged macOS x86_64 executable also passed the mock suite under Rosetta. Linux arm64 and x86_64 binaries were cross-compiled and inspected. Desktop integration was exercised on macOS arm64.

## Edit outcomes

A stale revision or selection stops the command. Cancellation stops pending work. A pending apply can race with cancellation or a timeout. The extension reports an unknown outcome when the connection can still carry a reply and the host has yet to confirm the edit. It never replays an apply. EOF or SIGTERM discards the session.
