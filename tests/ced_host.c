#include "extensions/manager.h"
#include "editor_bridge.h"
#include "jvm_project.h"
#include "lsp.h"
#include "cui_internal.h"
#include <sys/stat.h>
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

static const char *extension_id = "viniciusfonseca.java-kotlin";
static const char *sort_command = "viniciusfonseca.java-kotlin.sortImports";
static const char *main_command = "viniciusfonseca.java-kotlin.insertMain";

typedef struct {
    cui_ctx *ui;
    CedDocument doc;
    CedCommandContext context;
    unsigned commits;
    char message[CED_ERROR];
} TestEditor;

static CedCommandContext current_context(void *user) {
    return ((TestEditor *)user)->context;
}

static bool find_document(void *user, size_t index, CedExtensionDocument *out) {
    TestEditor *editor = user;
    if (index) return false;
    *out = (CedExtensionDocument){&editor->doc, ced_language(editor->doc.path),
        editor->context.anchor, editor->context.cursor};
    return true;
}

static bool commit(void *user, CedDocument *doc, uint64_t version,
                   const CedEditTransaction *transaction, char *error) {
    TestEditor *editor = user;
    cui_push_id_int(editor->ui, 1);
    bool ok = ced_editor_commit(editor->ui, "source", doc, version, transaction, error);
    cui_pop_id(editor->ui);
    if (ok && transaction->changed) editor->commits++;
    return ok;
}

static void message(void *user, const char *text) {
    snprintf(((TestEditor *)user)->message, CED_ERROR, "%s", text);
}

static void draw_editor(TestEditor *editor) {
    cui_push_id_int(editor->ui, 1);
    ced_editor_render(editor->ui, "source", &editor->doc,
        &(cui_code_editor_opts){.height = 300, .autofocus = true});
    cui_pop_id(editor->ui);
}

static void tick(TestEditor *editor, CedExtensions *host, double deadline) {
    assert(ced_ext_now() < deadline);
    cui_begin_frame(editor->ui, .016f);
    ced_extensions_tick(host);
    draw_editor(editor);
    cui_end_frame(editor->ui);
    editor->context.version = editor->doc.version;
    const struct timespec delay = {.tv_nsec = 5000000};
    nanosleep(&delay, NULL);
}

static void settle(TestEditor *editor, CedExtensions *host) {
    double deadline = ced_ext_now() + 6;
    do { tick(editor, host, deadline); } while (host->job.running || host->reload);
}

static void wait_command(TestEditor *editor, CedExtensions *host) {
    double deadline = ced_ext_now() + 6;
    while (*host->instances[0].command) tick(editor, host, deadline);
}

static void wait_stop(TestEditor *editor, CedExtensions *host) {
    double deadline = ced_ext_now() + 6;
    while (host->instances[0].process) tick(editor, host, deadline);
}

static void key(TestEditor *editor, CedExtensions *host, int modifiers) {
    double deadline = ced_ext_now() + 6;
    cui_input_key(editor->ui, CUI_KEY_Z, true, modifiers);
    tick(editor, host, deadline);
    cui_input_key(editor->ui, CUI_KEY_Z, false, 0);
    tick(editor, host, deadline);
}

static void run_from_palette(const CedCommands *commands, const CedCommandContext *captured,
                             const char *expected) {
    assert(commands->count == 2);
    cui_command items[2];
    for (size_t i = 0; i < commands->count; i++) {
        const CedCommandEntry *command = &commands->entries[i];
        assert(ced_command_condition(&command->when, captured));
        items[i] = (cui_command){.title = command->title, .group = command->category, .shortcut = command->shortcut};
    }
    cui_ctx *ui = cui_create(NULL);
    assert(ui);
    cui_set_viewport(ui, 800, 500, 1);
    cui_input_focus(ui, true);
    bool open = true;
    cui_begin_frame(ui, .016f);
    assert(ced_command_palette(ui, &open, items, 2) == -1);
    cui_end_frame(ui);
    cui_input_text(ui, "Sort Java/Kotlin Imports");
    cui_begin_frame(ui, .016f);
    assert(ced_command_palette(ui, &open, items, 2) == -1);
    cui_end_frame(ui);
    cui_input_key(ui, CUI_KEY_ENTER, true, 0);
    cui_begin_frame(ui, .016f);
    int selected = ced_command_palette(ui, &open, items, 2);
    cui_end_frame(ui);
    assert(selected >= 0 && selected < 2 && !strcmp(commands->entries[selected].id, expected));
    char error[CED_ERROR];
    assert(ced_commands_execute(commands, commands->entries[selected].id, captured, error));
    cui_destroy(ui);
}

static void invalid_manifest_variants(const char *package) {
    char filename[CED_PATH], error[CED_ERROR], original[4096];
    assert(ced_join(filename, sizeof filename, package, "ced-extension.json"));
    FILE *file = fopen(filename, "rb");
    assert(file);
    size_t length = fread(original, 1, sizeof original - 1, file);
    assert(feof(file) && !ferror(file) && length > 1);
    fclose(file);
    original[length] = 0;
    CedExtensionManifest manifest = {0};
    assert(ced_extension_manifest_parse(original, length, &manifest, error));
    assert(manifest.language_servers == (CED_EXT_LSP_JAVA | CED_EXT_LSP_KOTLIN));
    ced_extension_manifest_free(&manifest);
    const char *language = strstr(original, "\"kotlin\"");
    assert(language);
    char invalid_language[4096];
    snprintf(invalid_language, sizeof invalid_language, "%.*s\"java\"%s",
        (int)(language - original), original, language + strlen("\"kotlin\""));
    assert(!ced_extension_manifest_parse(invalid_language, strlen(invalid_language), &manifest, error));
    char mutated[8192];
    snprintf(mutated, sizeof mutated, "{\"name\":\"duplicate\",%s", original + 1);
    assert(!ced_extension_manifest_parse(mutated, strlen(mutated), &manifest, error));
    snprintf(mutated, sizeof mutated, "{\"unknown\":false,%s", original + 1);
    assert(!ced_extension_manifest_parse(mutated, strlen(mutated), &manifest, error));
    const char *path = strstr(original, "bin/darwin-arm64/java-kotlin");
    assert(path);
    snprintf(mutated, sizeof mutated, "%.*s../bin%s", (int)(path - original), original, path + 3);
    assert(!ced_extension_manifest_parse(mutated, strlen(mutated), &manifest, error));
}

static void fixture_file(const char *root, const char *name, const char *text) {
    char path[CED_PATH];
    assert(ced_join(path, sizeof path, root, name));
    FILE *file = fopen(path, "w");
    assert(file && fputs(text, file) >= 0 && !fclose(file));
}

static void check_jvm_project(const char *root, const char *java, const char *kotlin) {
    char directory[CED_PATH], detected[CED_PATH], error[CED_ERROR];
    CedJvmSettings defaults = {0}, settings;
    CedJvmBuild build;
    assert(ced_lsp_language_kind(java) == CED_LSP_JAVA);
    assert(ced_lsp_language_kind(kotlin) == CED_LSP_KOTLIN);
    assert(!strcmp(ced_language("settings.gradle.kts"), "kotlin"));
    cui__lang grammar = cui__lang_for("java");
    assert(grammar.block_comments && !strcmp(grammar.line_comment, "//"));
    cui__tok tokens[8];
    bool block = false;
    assert(cui__tokenize(&grammar, false, "record", 6, &block, tokens, 8) == 1);
    unsigned kind = tokens[0].kind;
    grammar = cui__lang_for("kotlin");
    assert(cui__tokenize(&grammar, false, "suspend", 7, &block, tokens, 8) == 1 && tokens[0].kind == kind);
    assert(cui__tokenize(&grammar, false, "ordinary", 8, &block, tokens, 8) == 1 && tokens[0].kind != kind);

    fixture_file(root, "pom.xml", "<project/>");
    assert(ced_preferences_project(root, &defaults, &settings, error));
    assert(ced_jvm_project(root, java, &settings, detected, &build, error));
    assert(build == CED_JVM_MAVEN && !strcmp(detected, root));

    assert(ced_join(directory, sizeof directory, root, ".ced") && !mkdir(directory, 0700));
    strcpy(defaults.gradle, "missing");
    fixture_file(root, ".ced/settings.json", "{\"maven.pomPath\":\"pom.xml\",\"gradle.projectPath\":\"\"}");
    assert(ced_preferences_project(root, &defaults, &settings, error));
    assert(!strcmp(settings.pom, "pom.xml") && !*settings.gradle);
    assert(ced_jvm_project(root, java, &settings, detected, &build, error) && build == CED_JVM_MAVEN);
    strcpy(settings.pom, "/tmp/not-a-project/pom.xml");
    assert(!ced_jvm_project(root, java, &settings, detected, &build, error));
    fixture_file(root, ".ced/work settings.xml", "<settings/>");
    fixture_file(root, ".ced/settings.json", "{\"maven.settingsPath\":\".ced/work settings.xml\"}");
    assert(ced_preferences_project(root, &defaults, &settings, error));
    assert(!strcmp(settings.maven_settings, ".ced/work settings.xml"));
    char maven_settings[CED_PATH];
    assert(ced_jvm_maven_settings(root, settings.maven_settings, maven_settings, error));
    assert(ced_join(detected, sizeof detected, root, ".ced/work settings.xml"));
    assert(!strcmp(maven_settings, detected));
    assert(ced_jvm_maven_settings(root, detected, maven_settings, error));
    assert(!ced_jvm_maven_settings(root, ".ced", maven_settings, error));
    assert(!ced_jvm_maven_settings(root, ".ced/missing.xml", maven_settings, error));
    assert(ced_jvm_maven_settings(root, "", maven_settings, error) && !*maven_settings);
    strcpy(defaults.maven_settings, detected);
    fixture_file(root, ".ced/settings.json", "{\"maven.settingsPath\":\"\"}");
    assert(ced_preferences_project(root, &defaults, &settings, error) && !*settings.maven_settings);
    fixture_file(root, ".ced/settings.json", "{\"gradle.projectPath\":\".\",\"gradle.javaHome\":\"/tools/jdk\"}");
    fixture_file(root, "settings.gradle.kts", "rootProject.name = \"example\"");
    assert(ced_preferences_project(root, &defaults, &settings, error));
    assert(ced_jvm_project(root, kotlin, &settings, detected, &build, error));
    assert(build == CED_JVM_GRADLE && !strcmp(detected, root));
    assert(!strcmp(settings.gradle_java, "/tools/jdk"));
    fixture_file(root, ".ced/settings.json", "{\"maven.pomPath\":33}");
    assert(!ced_preferences_project(root, &defaults, &settings, error));
}

int main(int argc, char **argv) {
    assert(argc == 3);
    invalid_manifest_variants(argv[1]);
    char root[CED_PATH], profile[CED_PATH], java[CED_PATH], kotlin[CED_PATH], error[CED_ERROR] = {0};
    assert(realpath(argv[2], root));
    assert(ced_join(profile, sizeof profile, root, "profile"));
    assert(ced_join(java, sizeof java, root, "Main.java"));
    assert(ced_join(kotlin, sizeof kotlin, root, "Música #1.kt"));
    FILE *file = fopen(java, "w");
    assert(file && fputs("class OnDisk {}\n", file) >= 0 && !fclose(file));
    file = fopen(kotlin, "w");
    assert(file && !fclose(file));
    check_jvm_project(root, java, kotlin);

    TestEditor editor = {0};
    assert(ced_document_open(&editor.doc, java, error));
    ced_document_identify(&editor.doc, 1);
    const char *before = "import zoo.B;\nimport aaa.A;\n\nclass Main { String s = \"é🙂\"; }\n";
    const char *after = "import aaa.A;\nimport zoo.B;\n\nclass Main { String s = \"é🙂\"; }\n";
    strcpy(editor.doc.text, before);
    assert(ced_document_note_change(&editor.doc) && editor.doc.version == 2);
    editor.context = (CedCommandContext){1, 1, 1, 0, 0, true, true, false, "text"};
    editor.ui = cui_create(NULL);
    assert(editor.ui);
    cui_set_viewport(editor.ui, 800, 500, 1);
    cui_input_focus(editor.ui, true);

    CedCommands commands = {0};
    CedExtensionHost callbacks = {&editor, current_context, find_document, commit, message, NULL};
    CedExtensions *host = ced_extensions_create(&commands, callbacks, profile, root);
    assert(host);
    settle(&editor, host);
    assert(host->count == 0 && !host->trusted);
    assert(ced_extensions_job(host, EXT_INSPECT, argv[1]));
    settle(&editor, host);
    assert(host->review_open);
    assert(ced_extensions_job(host, EXT_INSTALL, NULL));
    settle(&editor, host);
    assert(host->count == 1 && commands.count == 2);
    CedExtensionInstance *instance = &host->instances[0];
    assert(!ced_commands_execute(&commands, sort_command, &editor.context, error));
    assert(!instance->process);
    assert(!ced_extensions_language_provider(host, CED_EXT_LSP_JAVA));
    assert(ced_extensions_job(host, EXT_TRUST, NULL));
    settle(&editor, host);
    assert(host->trusted);
    assert(ced_extensions_language_provider(host, CED_EXT_LSP_JAVA) == &instance->record);
    assert(ced_extensions_language_provider(host, CED_EXT_LSP_KOTLIN) == &instance->record);
    editor.context.workspace_trusted = true;

    assert(!instance->process);
    run_from_palette(&commands, &editor.context, sort_command);
    wait_command(&editor, host);
    if (*instance->error) fprintf(stderr, "%s\n", instance->error);
    assert(instance->state == EXT_ACTIVE && !*instance->error);
    assert(editor.commits == 1 && !strcmp(editor.doc.text, after) && editor.doc.version == 3);
    assert(ced_document_dirty(&editor.doc));
    char persisted[128];
    file = fopen(java, "r");
    assert(file && fgets(persisted, sizeof persisted, file) && !fclose(file));
    assert(!strcmp(persisted, "class OnDisk {}\n"));
    key(&editor, host, CUI_MOD_PRIMARY);
    assert(!strcmp(editor.doc.text, before) && editor.doc.version == 4);
    key(&editor, host, CUI_MOD_PRIMARY | CUI_MOD_SHIFT);
    assert(!strcmp(editor.doc.text, after) && editor.doc.version == 5);
    key(&editor, host, CUI_MOD_PRIMARY);
    assert(ced_commands_execute(&commands, sort_command, &editor.context, error));
    editor.context.selection_version++;
    wait_command(&editor, host);
    assert(editor.commits == 1 && !strcmp(editor.doc.text, before));
    assert(strstr(instance->error, "StaleEditorContext") || strstr(instance->error, "context changed"));

    assert(ced_commands_execute(&commands, sort_command, &editor.context, error));
    editor.context.document_id = 2; /* Tab switch while the originating buffer remains open. */
    editor.context.selection_version++;
    wait_command(&editor, host);
    assert(editor.commits == 1 && strstr(instance->error, "context changed"));
    editor.context.document_id = 1;
    editor.context.selection_version++;

    assert(ced_commands_execute(&commands, sort_command, &editor.context, error));
    /* Change the live buffer and publish its new revision before the broker ticks. */
    strcat(editor.doc.text, "// typed while the command was pending\n");
    assert(ced_document_note_change(&editor.doc) && editor.doc.version == 7);
    wait_command(&editor, host);
    assert(editor.commits == 1 && editor.doc.version == 7);
    assert(strstr(instance->error, "StaleDocument") || strstr(instance->error, "changed"));

    assert(ced_commands_execute(&commands, sort_command, &editor.context, error));
    ced_extensions_workspace(host, root);
    settle(&editor, host);
    wait_stop(&editor, host);
    assert(editor.commits == 1);

    ced_document_free(&editor.doc);
    assert(ced_document_open(&editor.doc, kotlin, error));
    ced_document_identify(&editor.doc, 2);
    editor.context = (CedCommandContext){2, 1, 30, 0, 0, true, true, true, "text"};
    tick(&editor, host, ced_ext_now() + 6);
    instance = &host->instances[0];
    instance->record.granted = CED_EXT_READ;
    assert(ced_commands_execute(&commands, main_command, &editor.context, error));
    wait_command(&editor, host);
    assert(instance->state == EXT_FAILED && editor.commits == 1);
    assert(strstr(instance->error, "PermissionDenied") || strstr(instance->error, "grants"));
    wait_stop(&editor, host);

    instance->record.granted = CED_EXT_READ | CED_EXT_EDIT;
    ced_extensions_stop(instance, "restart");
    instance->state = EXT_REGISTERED;
    *instance->error = 0;
    assert(ced_commands_execute(&commands, main_command, &editor.context, error));
    wait_command(&editor, host);
    assert(instance->state == EXT_ACTIVE && !*instance->error && editor.commits == 2);
    assert(!strcmp(editor.doc.text, "fun main() {\n}\n"));

    assert(ced_extensions_job(host, EXT_DISABLE, extension_id));
    settle(&editor, host);
    wait_stop(&editor, host);
    assert(!host->instances[0].record.enabled && !commands.count);
    assert(!ced_extensions_language_provider(host, CED_EXT_LSP_JAVA));
    ced_extensions_destroy(host);
    ced_commands_free(&commands);
    cui_destroy(editor.ui);
    ced_document_free(&editor.doc);
    assert(ced_rpc_memory_used() == 0);
    puts("OK: native CED install, trust, lazy launch, grants, unsaved edits, undo/redo, stale buffers and selections, workspace invalidation and disable");
    return 0;
}
