package offload

// builtinFilters is the shipped filter set. It is adapted from rtk's TOML filter
// definitions (github.com/rtk-ai/rtk, Apache-2.0 — see THIRD-PARTY-NOTICES) with
// two systematic modifications:
//
//  1. Selectors are rewritten to OUTPUT-SHAPE signatures. rtk matches a SHELL
//     COMMAND (`^terraform\s+plan`); these match a signature tested against the
//     first few non-empty, trimmed lines of the tool output, which works whether or
//     not the request pairs the output with its call.
//
//     The command is NOT invisible to a proxy, as this comment used to claim: both
//     dialects carry the call and its result in the same request, joined by an id
//     (Anthropic `tool_use`/`tool_result`, OpenAI `tool_calls`/`tool_call_id`), and
//     schema.ToolCalls pairs them. cmdfilter therefore prefixes the selector with
//     `$ <command>` when the pairing exists, so a filter MAY be keyed on the command
//     — `match: '^\$ (rg|grep) '` — exactly as rtk's are. Shape signatures are still
//     the default because they fire on unpaired traffic too.
//
//  2. Where two tools' output is INDISTINGUISHABLE from the output alone,
//     rtk's per-command filters are merged into one (terraform/tofu plan;
//     terraform/tofu init; the five pulumi subcommands). Splitting them would
//     only make the shared selector ambiguous and the ordering arbitrary.
//
// Collapsing output to a one-line summary is the dangerous operation here — in a proxy
// the agent cannot re-run the command to discover the warning that got swallowed. There
// are TWO mechanisms that do it, and they are safe for DIFFERENT reasons. Both invariants
// are enforced by TestCollapseInvariants; a new filter must satisfy the one it uses.
//
//  1. `match_output` — pattern-matched success collapse. Safe because every rule carries
//     an `unless` guard naming the diagnostics that must veto the collapse. rtk ships 9
//     of 11 unguarded; ours are all guarded.
//  2. `on_empty` — fires only when `strip_lines_matching` removed EVERYTHING. Safe for a
//     structural reason instead: every strip list is an explicit allow-list of known
//     boilerplate prefixes with NO catch-all pattern, so an unrecognised line is simply
//     not stripped, the output is therefore not empty, and the collapse never fires. An
//     `unless` guard would be redundant. This is the stronger of the two designs — a guard
//     enumerates what to fear, an allow-list enumerates what is known-harmless and so is
//     safe against diagnostics nobody anticipated.
//
// 12 of the filters collapse via `on_empty` (apt, pytest, make, gcc, gradle, xcodebuild,
// pulumi, terraform-plan, terraform-init, liquibase, turbo, npm-install), so #2 is the
// common case, not the exception. Adding `.*` or `^.*$` to any strip list would silently
// void it — hence the test. See `apt`'s deliberate exclusion of `^debconf: ` for the shape
// of the reasoning: it would swallow "debconf: unable to initialize frontend".
//
// Line budgets come from the shared `cap` classes (dsl.Caps) rather than 25 hand-picked
// max_lines.
//
// Every filter ships inline tests; they run at load time (dsl.Registry.Load) and
// TestBuiltinFiltersSelfCheck asserts each test's input actually routes to its own
// filter — the check that makes a selector rewrite verifiable instead of hopeful.
//
// YAML strings are SINGLE-quoted so regex backslashes stay literal.
const builtinFilters = `
schema_version: 1
filters:

  pytest:
    description: keep failures + summary, drop passing noise
    family: tests
    priority: 10
    match: '(^=+ test session starts|^=+ (FAILURES|ERRORS|short test summary info)|^(FAILED|ERROR) \S+::|^\d+ (passed|failed|error)|^collected \d+ items)'
    strip_lines_matching:
      - '^\s*$'
      - ' PASSED'
      - '^\.+$'
      - '^=+ test session starts =+$'
      - '^platform \S+ -- Python'
      - '^cachedir:'
      - '^rootdir:'
      - '^plugins:'
    cap: buildlog
    on_empty: 'pytest: all passed'

  npm-install:
    description: collapse npm/yarn/pnpm install chatter
    family: pkg
    priority: 20
    match: '^(npm |yarn |pnpm |added \d|removed \d|up to date)'
    strip_lines_matching:
      - '^npm warn'
      - '^\s*$'
      - '^\s*[-|\\/] '
    match_output:
      - pattern: 'up to date'
        message: 'npm: up to date'
        unless: 'error|ERR!|deprecat|[1-9]\d* (\w+ )*severity'
    cap: list
    on_empty: 'install: ok'

  make:
    description: drop make directory chatter and no-op notices
    family: builds
    priority: 20
    match: '^(make(\[\d+\])?:|(gcc|g\+\+|cc|clang) )'
    strip_lines_matching:
      - '^make(\[\d+\])?: (Entering|Leaving) directory'
      - '^make(\[\d+\])?: Nothing to be done'
      - '^Nothing to be done'
      - '^\s*$'
    cap: buildlog
    on_empty: 'make: ok'

  gradle:
    description: strip Gradle progress and no-op tasks, keep tasks and errors
    family: builds
    priority: 20
    match: '^(> (Task|Configuring project|Resolving dependencies|Transform )|Starting a Gradle Daemon|BUILD (SUCCESSFUL|FAILED))'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^> Configuring project'
      - '^> Resolving dependencies'
      - '^> Transform '
      - '^Download(ing)?\s+http'
      - '^\s*<-+>\s*$'
      - '^> Task :.*UP-TO-DATE$'
      - '^> Task :.*NO-SOURCE$'
      - '^> Task :.*FROM-CACHE$'
      - '^Starting a Gradle Daemon'
      - '^Daemon will be stopped'
    truncate_lines_at: 200
    cap: buildlog
    on_empty: 'gradle: ok'

  xcodebuild:
    description: strip xcodebuild build phases and tool invocations, keep diagnostics
    family: builds
    priority: 20
    match: '^(note: Using new build system|CompileC |CompileSwift |Ld |CodeSign |PhaseScriptExecution |\*\* BUILD)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^CompileC\s'
      - '^CompileSwift\s'
      - '^Ld\s'
      - '^CreateBuildDirectory\s'
      - '^MkDir\s'
      - '^ProcessInfoPlistFile\s'
      - '^CopySwiftLibs\s'
      - '^CodeSign\s'
      - '^Signing Identity:'
      - '^RegisterWithLaunchServices'
      - '^Validate\s'
      - '^ProcessProductPackaging'
      - '^Touch\s'
      - '^LinkStoryboards'
      - '^CompileStoryboard'
      - '^CompileAssetCatalog'
      - '^GenerateDSYMFile'
      - '^PhaseScriptExecution'
      - '^PBXCp\s'
      - '^SetMode\s'
      - '^SetOwnerAndGroup\s'
      - '^Ditto\s'
      - '^CpResource\s'
      - '^CpHeader\s'
      - '^\s+cd\s+/'
      - '^\s+export\s'
      - '^\s+/Applications/Xcode'
      - '^\s+/usr/bin/'
      - '^\s+builtin-'
      - '^note: Using new build system'
    cap: buildlog
    on_empty: 'xcodebuild: ok'

  gcc:
    description: strip include traces and diagnostic counters, keep every error and warning
    family: builds
    # Lowest priority ON PURPOSE: this selector is a generic compiler-diagnostic shape
    # that also occurs inside make / swift / dotnet output. It is the fallback for
    # "some compiler said something" when no tool-specific filter claimed the output.
    priority: -10
    match: '^(In file included from|/usr/bin/ld:|collect2: error:|\S+: In function|\S+:\d+:(\d+:)? (error|warning|note):)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^\s+\|\s*$'
      - '^In file included from'
      - '^\s+from\s'
      - '^\d+ warnings? generated'
      - '^\d+ errors? generated'
    cap: buildlog
    on_empty: 'gcc: ok'

  swift-build:
    description: strip Compiling/Linking noise, collapse a clean build
    family: builds
    priority: 20
    match: '^(Compiling \S+ \S+\.swift|Building for (debugging|production)|Build complete!|\S+\.swift:\d+:\d+: (error|warning):)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^Compiling \S+ \S+\.swift'
      - '^Linking \S+$'
    match_output:
      - pattern: 'Build complete!'
        message: 'ok (build complete)'
        unless: 'warning:|error:|failed|Failed'
    cap: buildlog

  dotnet-build:
    description: strip MSBuild banners, collapse a clean build
    family: builds
    priority: 20
    match: '^(Microsoft \(R\) Build Engine|MSBuild version)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^Microsoft \(R\)'
      - '^Copyright \(C\)'
      - '^  Determining projects'
    match_output:
      # dotnet writes "0 Error(s)" even on success, so the guard names the
      # diagnostic FORM (error CS1002 / warning CS0168), not the word.
      - pattern: '0 Warning\(s\)\n\s+0 Error\(s\)'
        message: 'ok (build succeeded)'
        unless: '(error|warning) [A-Z]+\d|Build FAILED'
    cap: buildlog

  turbo:
    description: strip Turborepo cache status noise, keep task results
    family: builds
    priority: 20
    match: '^(cache (hit|miss|bypass)|\d+ packages in scope|> [^ ]+:[^ ]+$)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^\s*cache (hit|miss|bypass)'
      - '^\s*\d+ packages in scope'
      - '^\s*Tasks:\s+\d+'
      - '^\s*Duration:\s+'
      - '^\s*Remote caching (enabled|disabled)'
    truncate_lines_at: 200
    cap: buildlog
    on_empty: 'turbo: ok'

  nx:
    description: strip Nx task-graph banners, keep task output
    family: builds
    priority: 20
    match: '^(> +NX +|Nx \(powered by)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^\s*>\s*NX\s+Running target'
      - '^\s*>\s*NX\s+Nx read the output'
      - '^\s*>\s*NX\s+View logs'
      - '^———————'
      - '^\s+Nx \(powered by'
    truncate_lines_at: 200
    cap: buildlog

  terraform-plan:
    description: strip state-refresh and unchanged-resource noise from a terraform/tofu plan
    family: iac
    priority: 20
    match: '^(Acquiring state lock|Releasing state lock|Refreshing state|(Terraform|OpenTofu) (will perform|used the selected providers)|No changes\.)'
    strip_ansi: true
    strip_lines_matching:
      - '^Refreshing state'
      - '^\s*#.*unchanged'
      - '^\s*$'
      - '^Acquiring state lock'
      - '^Releasing state lock'
    cap: buildlog
    on_empty: 'plan: no changes detected'

  terraform-init:
    description: strip provider download spam from a terraform/tofu init
    family: iac
    priority: 20
    match: '^Initializing (the backend|provider plugins|modules)'
    strip_ansi: true
    strip_lines_matching:
      - '^- Downloading'
      - '^- Installing'
      - '^- Using previously-installed'
      - '^\s*$'
      - '^Initializing provider'
      - '^Initializing the backend'
      - '^Initializing modules'
    cap: list
    on_empty: 'init: ok'

  pulumi:
    description: strip pulumi banners, permalinks and per-resource progress rows
    family: iac
    priority: 20
    match: '^(Previewing (update|refresh|destroy)|Updating \(|Refreshing \(|Destroying \(|No stacks found|Please choose a stack|Current stack is)'
    strip_ansi: true
    match_output:
      - pattern: 'No stacks found'
        message: 'pulumi stack: empty'
        unless: 'error|Error'
    strip_lines_matching:
      - '^\s*$'
      - '^Previewing (update|refresh|destroy)'
      - '^Updating \('
      - '^Refreshing \('
      - '^Destroying \('
      - '^@ (Previewing (update|refresh|destroy)|Updating|Refreshing|Destroying)'
      - '^View in Browser'
      - '^View Live:'
      - '^Duration:'
      - '^Permalink:'
      - '^\s*Type\s+Name\s+'
      - '^Loading policy packs'
      - '^More information at:'
      - '^Use \x60pulumi '
      - '^Please choose a stack'
      - '^Current stack outputs \(0\):'
      - '^\s+No output values currently'
      - '^The resources in the stack have been deleted'
      - '^If you want to remove the stack completely'
      - '^\s+\+\s+.*\bcreating\s+\('
      - '^\s+~\s+.*\bupdating\s+\('
      - '^\s+-\s+.*\bdeleting\s+\('
      - '^\s+.*\brefreshing\s+\('
      - '^\s+pulumi:pulumi:Stack\s+\S+\s+running\s*$'
      - '^\s{4,}at\s+\S+\s*\('
      - '^\s{4,}at\s+/'
      - '^\s+at\s+processTicksAndRejections'
      - '^\s+promise:\s+Promise'
      - '^\s+\[Circular'
      - '^\s*<ref\s+\*\d'
    cap: buildlog
    on_empty: 'pulumi: no changes'

  liquibase:
    description: strip the ASCII banner, jar inventory and INFO chatter
    family: builds
    priority: 20
    match: '^(#{10,}$|Starting Liquibase at|Liquibase (Community|Open Source) )'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^Starting Liquibase at'
      - '^Liquibase (?:Community|Open Source)'
      - '^Liquibase Home:'
      - '^Java Home'
      - '^Libraries:'
      - '^\s*-\s+\S+\.jar'
      - '^INFO \[liquibase\.integration\]'
      - '^INFO \[liquibase\.core\] Reading resource'
      - '^INFO \[liquibase\.core\] Parsing'
      - '^(?:\[?INFO\]?\s*)?#+$'
      - '^\s*##'
    cap: inventory
    on_empty: 'liquibase: ok'

  ssh:
    description: strip ssh connection banners and debug1 flood, keep the remote output
    family: net
    priority: 20
    match: '^(Warning: Permanently added|debug1:|OpenSSH_|Pseudo-terminal)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^Warning: Permanently added'
      - '^Connection to .+ closed'
      - '^Authenticated to'
      - '^debug1:'
      - '^OpenSSH_'
      - '^Pseudo-terminal'
    truncate_lines_at: 200
    cap: inventory

  ping:
    description: drop per-packet replies, keep the statistics block
    family: net
    priority: 20
    match: '^(PING |Pinging )'
    strip_ansi: true
    strip_lines_matching:
      - '^PING '
      - '^Pinging '
      - '^\d+ bytes from '
      - '^Reply from .+: bytes='
      - '^\s*$'
    tail_lines: 4

  rsync:
    description: strip the file list and byte counters, collapse a clean sync
    family: net
    priority: 20
    match: '^(sending incremental file list|rsync: |rsync error:)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^sending incremental file list'
      - '^sent \d'
    match_output:
      - pattern: 'total size is'
        message: 'ok (synced)'
        unless: 'error|failed|No such file|Permission denied'
    cap: list

  bundle-install:
    description: strip cached-gem "Using" lines, collapse a complete bundle
    family: pkg
    priority: 20
    match: '^(Fetching gem metadata|Resolving dependencies\.|Using \S+ \d)'
    strip_ansi: true
    strip_lines_matching:
      - '^Using '
      - '^\s*$'
      - '^Fetching gem metadata'
      - '^Resolving dependencies'
    match_output:
      - pattern: 'Bundle complete!'
        message: 'ok bundle: complete'
        unless: 'error|Error|warning|conflict|Could not find'
      - pattern: 'Bundle updated!'
        message: 'ok bundle: updated'
        unless: 'error|Error|warning|conflict|Could not find'
    cap: list

  poetry-install:
    description: strip poetry download/install chatter, collapse an up-to-date lock
    family: pkg
    priority: 20
    match: '^(Installing dependencies from lock file|Creating virtualenv|Using virtualenv|\s*[•-] (Installing|Downloading) \S+ \()'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^  [-•] Downloading '
      - '^  [-•] Installing .* \('
      - '^[-•] Installing .* \('
      - '^[-•] Downloading '
      - '^Creating virtualenv'
      - '^Using virtualenv'
    match_output:
      - pattern: 'No dependencies to install or update|No changes\.'
        message: 'ok (up to date)'
        unless: 'error|Error|warning|SolverProblem'
    cap: list

  composer-install:
    description: strip composer download/install chatter, collapse a no-op install
    family: pkg
    priority: 20
    match: '^Loading composer repositories'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^  - Downloading '
      - '^  - Installing '
      - '^Loading composer'
      - '^Updating dependencies'
    match_output:
      - pattern: 'Nothing to install, update or remove'
        message: 'ok (up to date)'
        unless: 'error|Error|warning|abandoned|vulnerabilit'
    cap: list

  uv-sync:
    description: strip uv download/cache chatter, collapse an audited-only sync
    family: pkg
    priority: 20
    match: '^(Resolved \d+ packages|Downloading \S+\.whl|Using cached \S+\.whl|Audited \d+ package)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^\s+Downloading '
      - '^\s+Using cached '
      - '^\s+Preparing '
    match_output:
      - pattern: 'Audited \d+ package'
        message: 'ok (up to date)'
        unless: 'error|Error|warning|failed'
    cap: list

  apt:
    description: collapse apt/dpkg install boilerplate, keep errors and configuration prompts
    family: pkg
    priority: 20
    match: '^(Get:\d+ http|Selecting previously unselected package|Preparing to unpack |Unpacking |Setting up |Processing triggers for |Reading package lists|Building dependency tree)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^Get:\d+ http'
      - '^Selecting previously unselected package'
      - '^Preparing to unpack '
      - '^Unpacking '
      - '^Setting up '
      - '^Processing triggers for '
      - '^Reading package lists'
      - '^Building dependency tree'
      - '^Reading state information'
      - '^\(Reading database \.\.\.'
      - '^Fetched \d'
      - '^Need to get \d'
      - '^After this operation'
      # NOT '^debconf: ' — that swallows real diagnostics like
      # "debconf: unable to initialize frontend". Only the delaying notice is noise.
      - '^debconf: delaying package configuration'
      - '^update-alternatives: '
      - '^Created symlink '
      - '^Creating config file '
      - '^\d+ upgraded, \d+ newly installed'
    cap: list
    on_empty: 'apt: install ok'

  # Written FROM THE SELECTOR-MISS LEDGER, not from guesswork: on a Python workload pip
  # was the top-ranked unmatched output shape while poetry, uv, composer, bundler, apt and
  # brew were all covered. pip is the one installer nearly every Python task runs, so the
  # gap was in the set, not in the traffic.
  #
  # Only PROVABLY unactionable lines are stripped. The "WARNING:" prefix is deliberately
  # NOT stripped as a class — "WARNING: The script f2py is installed in '/x/bin' which is
  # not on PATH" is a real diagnostic the agent must see. Just the two fixed advisories
  # (root-user venv nag, pip-upgrade notice) go, plus per-package download chatter. The
  # "Successfully installed"/"Successfully uninstalled" manifest and every error survive.
  pip-install:
    description: strip pip download chatter and fixed advisories, keep the install manifest and errors
    family: pkg
    priority: 20
    match: '^(Collecting |Requirement already satisfied: |Installing collected packages: |Successfully installed |Looking in indexes: |WARNING: Running pip as the .root. user)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^Looking in indexes: '
      - '^Collecting '
      - '^Requirement already satisfied: '
      - '^\s+(Downloading|Using cached|Preparing metadata|Building wheel|Created wheel|Stored in directory|Getting requirements|Installing backend)'
      - '^\s+(Attempting uninstall|Uninstalling )'
      - '^Installing collected packages: '
      # The two fixed advisories. Both are constant text with no per-run information.
      - '^WARNING: Running pip as the .root. user'
      - '^\s*It is recommended to use a virtual environment instead'
      - '^\[notice\] A new release of pip is available'
      - '^\[notice\] To update, run: pip install --upgrade pip'
    match_output:
      # An install that only re-confirmed existing requirements has no result to report.
      - pattern: 'Requirement already satisfied'
        message: 'pip: requirements already satisfied'
        unless: 'ERROR|error:|Successfully installed|WARNING: The script|not on PATH'
    cap: list
    on_empty: 'pip: install ok'

  brew-install:
    description: strip brew download/pour chatter, collapse an already-installed formula
    family: pkg
    priority: 20
    match: '^(==> (Fetching|Downloading)|Warning: .+ is already installed)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^==> Downloading'
      - '^==> Pouring'
      - '^Already downloaded:'
      - '^###'
      - '^==> Fetching'
    match_output:
      - pattern: 'already installed'
        message: 'ok (already installed)'
        unless: 'Error|error:|failed'
    cap: list

  # Also from the selector-miss ledger. A TeX run's log is mostly fixed engine boilerplate
  # and absolute package paths out of the distribution tree; the SIGNAL is the
  # Overfull/Underfull diagnostics, "! ..." errors, missing-file notices and the output
  # summary. Only ABSOLUTE distribution paths are stripped — relative "(./input.tex" file
  # markers stay, because they are what attributes the following warnings to a source file.
  latex:
    description: strip TeX engine banner and distribution package paths, keep diagnostics and the output summary
    family: builds
    priority: 20
    match: '^(This is (pdfTeX|XeTeX|LuaTeX|e?TeX)|(/\S+/)?(pdf|xe|lua)?latex$|LaTeX2e <|entering extended mode)'
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^/\S+/(pdf|xe|lua)?latex$'
      - '^This is (pdfTeX|XeTeX|LuaTeX|e?TeX)'
      - '^\s*restricted \\write18 enabled\.'
      - '^entering extended mode'
      - '^LaTeX2e <'
      - '^L3 programming layer <'
      - '^Document Class: '
      - '^\(/(usr|var)/\S+$'
      - '^\(/(usr|var)/\S+\)+$'
      - '^\[\d+\{/\S+\}\]$'
      - '^\(see the transcript file for additional information\)'
      - '^Transcript written on '
    # buildlog (80), not list (20): a TeX log is a verbose transcript whose most
    # important line — "Output written on ..." / an "! ..." error — sits at the END.
    # Measured on a real 39-line pdflatex run, the list budget truncated the tail and
    # took the output summary with it.
    cap: buildlog
    on_empty: 'latex: ok'

  quarto-render:
    description: strip quarto render progress, collapse a successful render
    family: builds
    priority: 20
    match: '^processing file: '
    strip_ansi: true
    strip_lines_matching:
      - '^\s*$'
      - '^\s*processing file:'
      - '^\s*\d+/\d+\s'
      - '^\s*running'
      - '^\s*Rendering'
      - '^pandoc '
      - '^  Validating'
      - '^  Resolving'
    match_output:
      - pattern: 'Output created:'
        message: 'ok (output created)'
        unless: 'ERROR|error:|Error|WARNING'
    cap: list

tests:

  pytest:
    - name: all-green collapses
      input: "===== test session starts =====\ntests/a.py::t1 PASSED\ntests/a.py::t2 PASSED\n"
      expected: 'pytest: all passed'
    - name: failure kept
      input: "===== test session starts =====\ntests/a.py::t1 PASSED\nFAILED tests/a.py::t2 - AssertionError\n1 failed, 1 passed in 0.1s\n"
      expected: "FAILED tests/a.py::t2 - AssertionError\n1 failed, 1 passed in 0.1s"

  npm-install:
    - name: up to date collapses
      input: "up to date, audited 240 packages in 1s\nfound 0 vulnerabilities\n"
      expected: 'npm: up to date'
    - name: vulnerabilities not swallowed
      input: "up to date, audited 240 packages in 1s\n3 moderate severity vulnerabilities\n"
      expected: "up to date, audited 240 packages in 1s\n3 moderate severity vulnerabilities"
    - name: warn lines stripped
      input: "npm warn deprecated q@1.5.1\nadded 12 packages in 3s\n"
      expected: 'added 12 packages in 3s'

  make:
    - name: strips entering/leaving lines
      input: "make[1]: Entering directory '/home/user'\ngcc -O2 foo.c\nmake[1]: Leaving directory '/home/user'\n"
      expected: 'gcc -O2 foo.c'
    - name: strips blank lines
      input: "gcc -O2 foo.c\n\ngcc -O2 bar.c\n"
      expected: "gcc -O2 foo.c\ngcc -O2 bar.c"
    - name: nothing to be done collapses
      input: "make[1]: Entering directory '/home/user'\nmake[1]: Nothing to be done for 'all'.\nmake[1]: Leaving directory '/home/user'\n"
      expected: 'make: ok'
    - name: error kept
      input: "make[1]: Entering directory '/home/user'\nfoo.c:3:1: error: expected declaration\nmake[1]: *** [Makefile:4: foo.o] Error 1\n"
      expected: "foo.c:3:1: error: expected declaration\nmake[1]: *** [Makefile:4: foo.o] Error 1"

  gradle:
    - name: strips UP-TO-DATE tasks, keeps build result
      input: "> Configuring project :app\n> Task :app:compileJava UP-TO-DATE\n> Task :app:compileKotlin UP-TO-DATE\n> Task :app:test\n\n3 tests completed, 1 failed\n\nBUILD FAILED in 12s"
      expected: "> Task :app:test\n3 tests completed, 1 failed\nBUILD FAILED in 12s"
    - name: clean build preserved
      input: "BUILD SUCCESSFUL in 8s\n7 actionable tasks: 7 executed"
      expected: "BUILD SUCCESSFUL in 8s\n7 actionable tasks: 7 executed"
    - name: empty after stripping
      input: "> Configuring project :app\n"
      expected: 'gradle: ok'

  xcodebuild:
    - name: strips build phases, keeps errors and summary
      input: "note: Using new build system\nCompileSwift normal arm64 /d/App/ViewController.swift\n    cd /d/App\n    /Applications/Xcode.app/Contents/Developer/usr/bin/swift-frontend -c\nLd /d/Build/App normal arm64\n    cd /d/App\nCodeSign /d/Build/App.app\n    builtin-codesign --force --sign\n\n/d/App/ViewController.swift:42:9: error: use of unresolved identifier 'foo'\n/d/App/Model.swift:18:5: warning: variable 'x' was never used\n\n** BUILD FAILED **\n"
      expected: "/d/App/ViewController.swift:42:9: error: use of unresolved identifier 'foo'\n/d/App/Model.swift:18:5: warning: variable 'x' was never used\n** BUILD FAILED **"
    - name: clean build success
      input: "note: Using new build system\nCompileSwift normal arm64 /d/App/Main.swift\n    cd /d/App\nLd /d/Build/App normal arm64\nCodeSign /d/Build/App.app\n    builtin-codesign --force --sign\n\n** BUILD SUCCEEDED **\n"
      expected: '** BUILD SUCCEEDED **'
    - name: test results kept
      input: "note: Using new build system\nCompileSwift normal arm64 /d/AppTests/Tests.swift\n    cd /d/App\nTest Case '-[AppTests testExample]' passed (0.001 seconds).\nTest Case '-[AppTests testFailing]' failed (0.002 seconds).\nExecuted 2 tests, with 1 failure in 0.003 seconds\n"
      expected: "Test Case '-[AppTests testExample]' passed (0.001 seconds).\nTest Case '-[AppTests testFailing]' failed (0.002 seconds).\nExecuted 2 tests, with 1 failure in 0.003 seconds"

  gcc:
    - name: strips include chain, keeps errors and warnings
      input: "In file included from /usr/include/stdio.h:42:\n                 from main.c:1:\nmain.c:10:5: error: use of undeclared identifier 'foo'\n    foo();\n    ^\nmain.c:15:12: warning: unused variable 'x' [-Wunused-variable]\n    int x = 42;\n        ^\n2 warnings generated.\n1 error generated.\n"
      expected: "main.c:10:5: error: use of undeclared identifier 'foo'\n    foo();\n    ^\nmain.c:15:12: warning: unused variable 'x' [-Wunused-variable]\n    int x = 42;\n        ^"
    - name: linker error kept
      input: "/usr/bin/ld: /tmp/main.o: undefined reference to 'missing_func'\ncollect2: error: ld returned 1 exit status\n"
      expected: "/usr/bin/ld: /tmp/main.o: undefined reference to 'missing_func'\ncollect2: error: ld returned 1 exit status"

    - name: In-function header routes and diagnostics survive
      input: "/tmp/spherepeak.c: In function 'main':\n/tmp/spherepeak.c:12:9: warning: unused variable 'r' [-Wunused-variable]\n   12 |     int r = 0;\n      |         ^\n1 warning generated.\n"
      expected: "/tmp/spherepeak.c: In function 'main':\n/tmp/spherepeak.c:12:9: warning: unused variable 'r' [-Wunused-variable]\n   12 |     int r = 0;\n      |         ^"

  swift-build:
    - name: successful build collapses
      input: "Build complete! (4.21s)\n"
      expected: 'ok (build complete)'
    - name: build errors pass through after stripping noise
      input: "Compiling MyApp MyApp.swift\n/h/Sources/MyApp/main.swift:5:1: error: use of unresolved identifier 'foo'\nfoo()\n^~~\nLinking MyApp\nerror: build had 1 command failure\n"
      expected: "/h/Sources/MyApp/main.swift:5:1: error: use of unresolved identifier 'foo'\nfoo()\n^~~\nerror: build had 1 command failure"
    - name: warnings not swallowed when Build complete present
      input: "Compiling MyApp MyFile.swift\n/path/to/MyFile.swift:42:10: warning: unused variable 'x'\nBuild complete! (with warnings)\n"
      expected: "/path/to/MyFile.swift:42:10: warning: unused variable 'x'\nBuild complete! (with warnings)"

  dotnet-build:
    - name: successful build collapses
      input: "Microsoft (R) Build Engine version 17.8.3\nCopyright (C) Microsoft Corporation. All rights reserved.\n\n  Determining projects to restore...\n  MyApp -> /h/MyApp/bin/Debug/net8.0/MyApp.dll\n\nBuild succeeded.\n    0 Warning(s)\n    0 Error(s)\n\nTime Elapsed 00:00:02.34\n"
      expected: 'ok (build succeeded)'
    - name: build with warnings not collapsed
      input: "Microsoft (R) Build Engine version 17.8.3\nCopyright (C) Microsoft Corporation. All rights reserved.\n\n  MyApp -> /h/MyApp/bin/Debug/net8.0/MyApp.dll\n\nBuild succeeded.\n    3 Warning(s)\n    0 Error(s)\n\nTime Elapsed 00:00:01.87\n"
      expected: "  MyApp -> /h/MyApp/bin/Debug/net8.0/MyApp.dll\nBuild succeeded.\n    3 Warning(s)\n    0 Error(s)\nTime Elapsed 00:00:01.87"
    - name: zero-count warning line does not swallow a real diagnostic
      input: "Microsoft (R) Build Engine version 17.8.3\nsrc/Program.cs(9,5): warning CS0168: variable declared but never used\nBuild succeeded.\n    0 Warning(s)\n    0 Error(s)\n"
      expected: "src/Program.cs(9,5): warning CS0168: variable declared but never used\nBuild succeeded.\n    0 Warning(s)\n    0 Error(s)"
    - name: build errors pass through
      input: "Microsoft (R) Build Engine version 17.8.3\nCopyright (C) Microsoft Corporation. All rights reserved.\n\n  Determining projects to restore...\nsrc/Program.cs(10,5): error CS1002: ; expected [/h/MyApp/MyApp.csproj]\n\nBuild FAILED.\n    0 Warning(s)\n    1 Error(s)\n"
      expected: "src/Program.cs(10,5): error CS1002: ; expected [/h/MyApp/MyApp.csproj]\nBuild FAILED.\n    0 Warning(s)\n    1 Error(s)"

  turbo:
    - name: strips cache noise, keeps task output
      input: " cache hit, replaying logs abc123\n cache miss, executing abc456\n\n3 packages in scope\n\n> myapp:build\n\nCompiled successfully.\n\nTasks:    2 successful, 2 total (1 cached)\nDuration: 3.2s"
      expected: "> myapp:build\nCompiled successfully."
    - name: preserves error output
      input: "> myapp:lint\n\nError: src/index.ts(5,1): error TS2304\n\nTasks:    0 successful, 1 total\nDuration: 1.1s"
      expected: "> myapp:lint\nError: src/index.ts(5,1): error TS2304"
    - name: empty after stripping
      input: " cache hit, replaying logs abc\n\n"
      expected: 'turbo: ok'

  nx:
    - name: strips Nx noise, keeps build output
      input: "\n   > NX   Running target build for project myapp\n\n———————————————————————————————————————\nCompiled successfully.\nOutput: dist/apps/myapp\n\n   > NX   View logs at /tmp/.nx/runs/abc123\n\n   Nx (powered by computation caching)\n"
      expected: "Compiled successfully.\nOutput: dist/apps/myapp"
    - name: preserves error output
      input: "   > NX   Running target build for project myapp\n\nERROR: Cannot find module '@myapp/shared'\nFailed at step: build\n\n   > NX   View logs at /tmp/.nx/runs/abc\n"
      expected: "ERROR: Cannot find module '@myapp/shared'\nFailed at step: build"

  terraform-plan:
    - name: strips refresh and lock noise
      input: "Acquiring state lock. This may take a few moments...\nRefreshing state... [id=vpc-abc]\nRefreshing state... [id=sg-123]\nReleasing state lock. This may take a few moments...\n\nTerraform will perform the following actions:\n\n  # aws_instance.web will be created\n  + resource \"aws_instance\" \"web\" {}\n\nPlan: 1 to add, 0 to change, 0 to destroy.\n"
      expected: "Terraform will perform the following actions:\n  # aws_instance.web will be created\n  + resource \"aws_instance\" \"web\" {}\nPlan: 1 to add, 0 to change, 0 to destroy."
    - name: opentofu plan also handled
      input: "Acquiring state lock. This may take a few moments...\nRefreshing state... [id=vpc-abc123]\nReleasing state lock. This may take a few moments...\n\nOpenTofu will perform the following actions:\n\n  # aws_instance.web will be created\n\nPlan: 1 to add, 0 to change, 0 to destroy.\n"
      expected: "OpenTofu will perform the following actions:\n  # aws_instance.web will be created\nPlan: 1 to add, 0 to change, 0 to destroy."
    - name: no-changes result preserved
      input: "Refreshing state... [id=vpc-abc]\nNo changes. Your infrastructure matches the configuration.\n"
      expected: 'No changes. Your infrastructure matches the configuration.'
    - name: on_empty when all noise stripped
      input: "Refreshing state... [id=vpc-abc]\nAcquiring state lock. This may take a few moments...\nReleasing state lock. This may take a few moments...\n"
      expected: 'plan: no changes detected'
    - name: unchanged-resource comments dropped, changes kept
      input: "Refreshing state... [id=vpc-abc]\nTerraform will perform the following actions:\n  # aws_s3_bucket.a will be created\n  # (12 unchanged attributes hidden)\n  # (3 unchanged blocks hidden)\nPlan: 1 to add, 0 to change, 0 to destroy.\n"
      expected: "Terraform will perform the following actions:\n  # aws_s3_bucket.a will be created\nPlan: 1 to add, 0 to change, 0 to destroy."

  terraform-init:
    - name: strips downloading/installing lines
      input: "Initializing the backend...\nInitializing provider plugins...\n- Downloading hashicorp/aws 5.0.0...\n- Installing hashicorp/aws 5.0.0...\n- Using previously-installed hashicorp/random 3.5.1\n\nOpenTofu has been successfully initialized!\n"
      expected: 'OpenTofu has been successfully initialized!'
    - name: on_empty when all noise stripped
      input: "Initializing the backend...\nInitializing provider plugins...\n- Using previously-installed hashicorp/aws 5.0.0\n\n"
      expected: 'init: ok'
    - name: init error kept
      input: "Initializing the backend...\nError: Failed to get existing workspaces: S3 bucket does not exist.\n"
      expected: 'Error: Failed to get existing workspaces: S3 bucket does not exist.'

  pulumi:
    - name: preview strips header, url and duration noise
      input: "Previewing update (dev)\n\nView in Browser (Ctrl+O): https://app.pulumi.com/org/p/dev/previews/abc\n\n     Type                          Name              Plan\n +   pulumi:pulumi:Stack           my-proj-dev       create\n +   └─ aws:s3:Bucket              my-bucket         create\n\nResources:\n    + 2 to create\n\nDuration: 3s\n"
      expected: " +   pulumi:pulumi:Stack           my-proj-dev       create\n +   └─ aws:s3:Bucket              my-bucket         create\nResources:\n    + 2 to create"
    - name: up strips header and url banner
      input: "Updating (dev)\n\nView in Browser (Ctrl+O): https://app.pulumi.com/org/p/dev/updates/42\n\n     Type                          Name              Status\n +   pulumi:pulumi:Stack           my-proj-dev       created\n\nOutputs:\n    bucket_name: \"my-bucket-abc123\"\n\nResources:\n    + 2 created\n\nDuration: 15s\n"
      expected: " +   pulumi:pulumi:Stack           my-proj-dev       created\nOutputs:\n    bucket_name: \"my-bucket-abc123\"\nResources:\n    + 2 created"
    - name: refresh passes drift rows
      input: "Refreshing (dev)\n\nView in Browser (Ctrl+O): https://app.pulumi.com/org/p/dev/updates/44\n\n     Type                          Name              Status\n ~   aws:s3:Bucket                 my-bucket         refreshed\n\nResources:\n    ~ 1 updated\n\nDuration: 4s\n"
      expected: " ~   aws:s3:Bucket                 my-bucket         refreshed\nResources:\n    ~ 1 updated"
    - name: destroy strips header and url banner
      input: "Destroying (dev)\n\nView in Browser (Ctrl+O): https://app.pulumi.com/org/p/dev/updates/43\n\n     Type                          Name              Status\n -   pulumi:pulumi:Stack           my-proj-dev       deleted\n\nResources:\n    - 2 deleted\n\nDuration: 7s\n"
      expected: " -   pulumi:pulumi:Stack           my-proj-dev       deleted\nResources:\n    - 2 deleted"
    - name: on_empty when only noise lines present
      input: "Previewing update (dev)\n\nView in Browser (Ctrl+O): https://app.pulumi.com/org/p/dev/previews/abc\n\nDuration: 1s\n"
      expected: 'pulumi: no changes'
    - name: no stacks collapses
      input: "No stacks found in the current workspace.\n"
      expected: 'pulumi stack: empty'
    - name: stack identity kept, prompt noise stripped
      input: "Please choose a stack, or create a new one:\nCurrent stack is dev:\n    Managed by my-user\n    Owner: my-org\n\nCurrent stack resources (3):\n    TYPE                          NAME\n    pulumi:pulumi:Stack           my-proj-dev\n"
      expected: "Current stack is dev:\n    Managed by my-user\n    Owner: my-org\nCurrent stack resources (3):\n    TYPE                          NAME\n    pulumi:pulumi:Stack           my-proj-dev"
    - name: js stack frames pruned, error message kept
      input: "Updating (dev)\n\n    error: Error: connect ECONNREFUSED 127.0.0.1:443\n        at TCPConnectWrap.afterConnect (node:net:1595:16)\n        at /home/u/node_modules/@pulumi/runtime/invoke.js:120:23\n        at processTicksAndRejections (node:internal/process:95:5)\n\nDuration: 2s\n"
      expected: '    error: Error: connect ECONNREFUSED 127.0.0.1:443'

  liquibase:
    - name: strip ascii banner and info logs
      input: "####################################################\n##   _     _             _ _                      ##\n####################################################\nStarting Liquibase at 10:12:11 (version 4.29.1)\nLiquibase Version: 4.29.1\nLiquibase Open Source 4.29.1 by Liquibase\nINFO [liquibase.integration] Starting command\nINFO [liquibase.core] Reading resource db/changelog.xml\nINFO [liquibase.core] Parsing db/changelog.xml\nRunning Changeset: filepath::id::author\nChangeset filepath::id::author ran successfully\n"
      expected: "Liquibase Version: 4.29.1\nRunning Changeset: filepath::id::author\nChangeset filepath::id::author ran successfully"
    - name: strip jar inventory, keep version line
      input: "####################################################\n##   _     _             _ _                      ##\n####################################################\nStarting Liquibase at 13:45:24 using Java 17.0.15\nLiquibase Home: /opt/liquibase\nJava Home /usr/lib/jvm/jdk-17 (Version 17.0.15)\nLibraries:\n  - internal/lib/commons-io.jar: Apache Commons IO 2.17.0\n  - internal/lib/picocli.jar: picocli 4.7.6\n\nLiquibase Version: 4.30.0\nLiquibase Open Source 4.30.0 by Liquibase\n"
      expected: 'Liquibase Version: 4.30.0'
    - name: keep status and error lines
      input: "####################################################\n##   _     _             _ _                      ##\n####################################################\nStarting Liquibase at 10:00:00 (version 4.30.0)\nLiquibase Version: 4.30.0\nLiquibase Open Source 4.30.0 by Liquibase\nHR@jdbc:oracle:thin:@localhost:1523:XE is up to date\nLiquibase command 'status' was executed successfully.\n"
      expected: "Liquibase Version: 4.30.0\nHR@jdbc:oracle:thin:@localhost:1523:XE is up to date\nLiquibase command 'status' was executed successfully."

  ssh:
    - name: strips connection banners, keeps command output
      input: "Warning: Permanently added '192.168.1.10' (ED25519) to the list of known hosts.\n\ntotal 32\ndrwxr-xr-x 4 user user 4096 Mar 10 12:00 app\n-rw-r--r-- 1 user user 1234 Mar 10 11:00 config.yaml\n\nConnection to 192.168.1.10 closed.\n"
      expected: "total 32\ndrwxr-xr-x 4 user user 4096 Mar 10 12:00 app\n-rw-r--r-- 1 user user 1234 Mar 10 11:00 config.yaml"
    - name: verbose debug lines stripped
      input: "debug1: Connecting to host.example.com port 22.\ndebug1: Connection established.\nAuthenticated to host.example.com ([1.2.3.4]:22).\nuptime: 12:00:00 up 42 days, load average: 0.10, 0.15, 0.12\nConnection to host.example.com closed.\n"
      expected: 'uptime: 12:00:00 up 42 days, load average: 0.10, 0.15, 0.12'
    - name: remote failure kept
      input: "debug1: Connecting to host.example.com port 22.\nPermission denied (publickey).\n"
      expected: 'Permission denied (publickey).'

  ping:
    - name: success keeps summary only
      input: "PING example.com (93.184.216.34): 56 data bytes\n64 bytes from 93.184.216.34: icmp_seq=0 ttl=56 time=14.2 ms\n64 bytes from 93.184.216.34: icmp_seq=1 ttl=56 time=13.8 ms\n64 bytes from 93.184.216.34: icmp_seq=2 ttl=56 time=14.1 ms\n64 bytes from 93.184.216.34: icmp_seq=3 ttl=56 time=13.9 ms\n\n--- example.com ping statistics ---\n4 packets transmitted, 4 packets received, 0.0% packet loss\nround-trip min/avg/max/stddev = 13.8/14.0/14.2/0.2 ms\n"
      expected: "--- example.com ping statistics ---\n4 packets transmitted, 4 packets received, 0.0% packet loss\nround-trip min/avg/max/stddev = 13.8/14.0/14.2/0.2 ms"
    - name: windows format keeps stats block only
      input: "Pinging 192.0.2.1 with 32 bytes of data:\nReply from 192.0.2.1: bytes=32 time=14ms TTL=56\nReply from 192.0.2.1: bytes=32 time=13ms TTL=56\nReply from 192.0.2.1: bytes=32 time=14ms TTL=56\nReply from 192.0.2.1: bytes=32 time=13ms TTL=56\n\nPing statistics for 192.0.2.1:\n    Packets: Sent = 4, Received = 4, Lost = 0 (0% loss),\nApproximate round trip times in milli-seconds:\n    Minimum = 13ms, Maximum = 14ms, Average = 13ms\n"
      expected: "Ping statistics for 192.0.2.1:\n    Packets: Sent = 4, Received = 4, Lost = 0 (0% loss),\nApproximate round trip times in milli-seconds:\n    Minimum = 13ms, Maximum = 14ms, Average = 13ms"
    - name: unreachable host passes error through
      input: "PING unreachable.example.com (192.0.2.1): 56 data bytes\nRequest timeout for icmp_seq 0\nRequest timeout for icmp_seq 1\n\n--- unreachable.example.com ping statistics ---\n2 packets transmitted, 0 packets received, 100.0% packet loss\n"
      expected: "Request timeout for icmp_seq 0\nRequest timeout for icmp_seq 1\n--- unreachable.example.com ping statistics ---\n2 packets transmitted, 0 packets received, 100.0% packet loss"

  rsync:
    - name: successful sync collapses
      input: "sending incremental file list\n./\nfile1.txt\nfile2.txt\n\nsent 1,234 bytes  received 42 bytes  2,552.00 bytes/sec\ntotal size is 98,765  speedup is 77.31\n"
      expected: 'ok (synced)'
    - name: error lines pass through
      input: "sending incremental file list\nrsync: [Receiver] mkdir \"/remote/path\" failed: Permission denied (13)\nrsync error: error in file system (code 11) at receiver.c(741)\n"
      expected: "rsync: [Receiver] mkdir \"/remote/path\" failed: Permission denied (13)\nrsync error: error in file system (code 11) at receiver.c(741)"
    - name: errors not swallowed when total size present
      input: "rsync: [sender] error\nerror in rsync protocol data stream (code 12)\nsent 100 bytes  received 200 bytes  60.00 bytes/sec\ntotal size is 1000  speedup is 3.33\n"
      expected: "rsync: [sender] error\nerror in rsync protocol data stream (code 12)\ntotal size is 1000  speedup is 3.33"

  bundle-install:
    - name: all cached collapses
      input: "Using bundler 2.5.6\nUsing rake 13.1.0\nUsing ast 2.4.2\nUsing minitest 5.22.2\nBundle complete! 85 Gemfile dependencies, 200 gems now installed.\nUse 'bundle info [gemname]' to see where a bundled gem is installed.\n"
      expected: 'ok bundle: complete'
    - name: mixed install collapses
      input: "Fetching gem metadata from https://rubygems.org/.........\nResolving dependencies...\nUsing rake 13.1.0\nFetching rspec 3.13.0\nInstalling rspec 3.13.0\nBundle complete! 85 Gemfile dependencies, 202 gems now installed.\n"
      expected: 'ok bundle: complete'
    - name: update output collapses
      input: "Fetching gem metadata from https://rubygems.org/.........\nResolving dependencies...\nUsing rake 13.1.0\nInstalling rspec 3.14.0 (was 3.13.0)\nBundle updated!\n"
      expected: 'ok bundle: updated'
    - name: conflict not swallowed by Bundle complete
      input: "Fetching gem metadata from https://rubygems.org/.........\nwarning: rack 3.0 conflicts with rails 6.1\nBundle complete! 5 Gemfile dependencies, 9 gems now installed.\n"
      expected: "warning: rack 3.0 conflicts with rails 6.1\nBundle complete! 5 Gemfile dependencies, 9 gems now installed."

  poetry-install:
    - name: up to date collapses
      input: "Installing dependencies from lock file\n\nNo dependencies to install or update\n"
      expected: 'ok (up to date)'
    - name: bullet syntax collapses
      input: "• Installing requests (2.31.0)\n• Installing certifi (2023.11.17)\n\nNo changes.\n"
      expected: 'ok (up to date)'
    - name: install strips download lines
      input: "Installing dependencies from lock file\n\n  - Downloading requests-2.31.0-py3-none-any.whl (62.6 kB)\n  - Installing certifi (2023.11.17)\n  - Installing charset-normalizer (3.3.2)\n  - Installing requests (2.31.0)\n\nWriting lock file\n"
      expected: "Installing dependencies from lock file\nWriting lock file"
    - name: solver error not swallowed
      input: "Installing dependencies from lock file\nSolverProblemError: version solving failed\nNo changes.\n"
      expected: "Installing dependencies from lock file\nSolverProblemError: version solving failed\nNo changes."

  composer-install:
    - name: nothing to do collapses
      input: "Loading composer repositories with package information\nUpdating dependencies\nLock file operations: 0 installs, 0 updates, 0 removals\nNothing to install, update or remove\nGenerating autoload files\n"
      expected: 'ok (up to date)'
    - name: install strips download lines
      input: "Loading composer repositories with package information\nUpdating dependencies\n  - Downloading symfony/console (v6.4.0)\n  - Installing symfony/console (v6.4.0): Extracting archive\n  - Downloading psr/log (3.0.0)\nWriting lock file\nGenerating autoload files\n"
      expected: "Writing lock file\nGenerating autoload files"
    - name: abandoned package warning not swallowed
      input: "Loading composer repositories with package information\nWarning: Package foo/bar is abandoned, use baz/qux instead.\nNothing to install, update or remove\n"
      expected: "Warning: Package foo/bar is abandoned, use baz/qux instead.\nNothing to install, update or remove"

  uv-sync:
    - name: audited packages collapses
      input: "Resolved 42 packages in 123ms\nAudited 42 packages in 0.05ms\n"
      expected: 'ok (up to date)'
    - name: install strips download and cached lines
      input: "  Downloading requests-2.31.0-py3-none-any.whl (62.6 kB)\n  Using cached certifi-2023.11.17-py3-none-any.whl (162 kB)\n  Preparing packages...\nInstalled 5 packages in 23ms\n + certifi==2023.11.17\n + requests==2.31.0\n"
      expected: "Installed 5 packages in 23ms\n + certifi==2023.11.17\n + requests==2.31.0"
    - name: failure not swallowed by Audited
      input: "Resolved 42 packages in 123ms\nwarning: 'pytest' was not found in the lockfile\nAudited 42 packages in 0.05ms\n"
      expected: "Resolved 42 packages in 123ms\nwarning: 'pytest' was not found in the lockfile\nAudited 42 packages in 0.05ms"

  apt:
    - name: pure install boilerplate collapses
      input: "Setting up libx11-data (2:1.8.7-1build1) ...\nSetting up perl-modules-5.38 (5.38.2-3.2ubuntu0.3) ...\nSetting up git (1:2.43.0-1ubuntu7.3) ...\nProcessing triggers for libc-bin (2.39-0ubuntu8.6) ...\n"
      expected: 'apt: install ok'
    - name: errors and prompts kept
      input: "Reading package lists...\nBuilding dependency tree...\nGet:1 http://archive.ubuntu.com/ubuntu noble/main amd64 libfoo amd64 1.0 [12 kB]\nE: Unable to locate package libnope\nSetting up libfoo (1.0) ...\n"
      expected: 'E: Unable to locate package libnope'
    - name: dpkg failure kept
      input: "Preparing to unpack .../libbar_2.0_amd64.deb ...\nUnpacking libbar (2.0) ...\ndpkg: error processing archive libbar_2.0_amd64.deb (--unpack):\n trying to overwrite '/usr/lib/libbar.so', which is also in package libbaz\nSetting up libfoo (1.0) ...\n"
      expected: "dpkg: error processing archive libbar_2.0_amd64.deb (--unpack):\n trying to overwrite '/usr/lib/libbar.so', which is also in package libbaz"

  pip-install:
    - name: strips collect/download chatter, keeps the manifest
      input: "Collecting numpy==2.3.4\n  Downloading numpy-2.3.4-cp313.whl (16.6 MB)\nInstalling collected packages: numpy\nSuccessfully installed numpy-2.3.4\n"
      expected: 'Successfully installed numpy-2.3.4'
    - name: fixed advisories stripped, manifest kept
      input: "Successfully installed numpy-2.3.4\nWARNING: Running pip as the 'root' user can result in broken permissions.\n\n[notice] A new release of pip is available: 25.2 -> 26.2.1\n[notice] To update, run: pip install --upgrade pip\n"
      expected: 'Successfully installed numpy-2.3.4'
    - name: a real PATH warning is not swallowed
      input: "Collecting numpy\nSuccessfully installed numpy-2.3.4\nWARNING: The script f2py is installed in '/usr/local/bin' which is not on PATH.\n"
      expected: "Successfully installed numpy-2.3.4\nWARNING: The script f2py is installed in '/usr/local/bin' which is not on PATH."
    - name: errors pass through
      input: "Collecting nosuchpkg\nERROR: Could not find a version that satisfies the requirement nosuchpkg\nERROR: No matching distribution found for nosuchpkg\n"
      expected: "ERROR: Could not find a version that satisfies the requirement nosuchpkg\nERROR: No matching distribution found for nosuchpkg"
    - name: already-satisfied install collapses
      input: "Requirement already satisfied: numpy in /usr/lib/python3/site-packages (2.3.4)\nRequirement already satisfied: six in /usr/lib/python3/site-packages (1.17.0)\n"
      expected: 'pip: requirements already satisfied'

  brew-install:
    - name: already installed collapses
      input: "Warning: jq 1.7.1 is already installed and up-to-date.\nTo reinstall 1.7.1, run:\n  brew reinstall jq\n"
      expected: 'ok (already installed)'
    - name: install strips download lines
      input: "==> Fetching jq\n==> Downloading https://ghcr.io/v2/homebrew/core/jq/blobs/sha256:abc\n######################################################################## 100.0%\n==> Pouring jq-1.7.1.arm64_sonoma.bottle.tar.gz\n==> Summary\n/opt/homebrew/Cellar/jq/1.7.1: 18 files, 1.2MB\n"
      expected: "==> Summary\n/opt/homebrew/Cellar/jq/1.7.1: 18 files, 1.2MB"
    - name: error not swallowed by already installed
      input: "Warning: jq 1.7.1 is already installed and up-to-date.\nError: Could not link jq: permission denied\n"
      expected: "Warning: jq 1.7.1 is already installed and up-to-date.\nError: Could not link jq: permission denied"

  latex:
    - name: strips engine banner and distribution paths, keeps diagnostics
      input: "This is pdfTeX, Version 3.141592653 (TeX Live 2023/Debian)\n restricted \\write18 enabled.\nentering extended mode\n(./main.tex\nLaTeX2e <2023-11-01> patch level 1\n(/usr/share/texlive/texmf-dist/tex/latex/base/article.cls\nDocument Class: article 2023/05/17 v1.4n Standard LaTeX document class\n(./input.tex\nOverfull \\hbox (0.10312pt too wide) in paragraph at lines 5--6\nOutput written on main.pdf (5 pages, 29584 bytes).\nTranscript written on main.log.\n"
      expected: "(./main.tex\n(./input.tex\nOverfull \\hbox (0.10312pt too wide) in paragraph at lines 5--6\nOutput written on main.pdf (5 pages, 29584 bytes)."
    - name: errors pass through
      input: "This is pdfTeX, Version 3.141592653 (TeX Live 2023/Debian)\nentering extended mode\n(./main.tex\n! Undefined control sequence.\nl.7 \\bogus\n! Emergency stop.\n"
      expected: "(./main.tex\n! Undefined control sequence.\nl.7 \\bogus\n! Emergency stop."
    - name: font map page marker stripped
      input: "This is pdfTeX, Version 3.141592653 (TeX Live 2023/Debian)\nentering extended mode\n[1{/var/lib/texmf/fonts/map/pdftex/updmap/pdftex.map}]\nOutput written on main.pdf (1 page, 100 bytes).\n"
      expected: 'Output written on main.pdf (1 page, 100 bytes).'
    - name: nothing but boilerplate collapses
      input: "This is pdfTeX, Version 3.141592653 (TeX Live 2023/Debian)\n restricted \\write18 enabled.\nentering extended mode\nTranscript written on main.log.\n"
      expected: 'latex: ok'

  quarto-render:
    - name: success collapses
      input: "processing file: index.qmd\n  Validating schema\n  Resolving resources\npandoc to html5\nOutput created: _site/index.html\n"
      expected: 'ok (output created)'
    - name: error passes through
      input: "processing file: broken.qmd\n  Validating schema\nERROR: Render failed\n\ncaused by:\n  syntax error at line 10\n"
      expected: "ERROR: Render failed\ncaused by:\n  syntax error at line 10"
    - name: warning not swallowed by Output created
      input: "processing file: index.qmd\nWARNING: unable to resolve crossref @fig-1\nOutput created: _site/index.html\n"
      expected: "WARNING: unable to resolve crossref @fig-1\nOutput created: _site/index.html"
`
