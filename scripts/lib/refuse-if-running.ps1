# scripts/lib/refuse-if-running.ps1 — Windows/PowerShell counterpart of
# scripts/lib/refuse-if-running.sh.
#
# KEEP IN SYNC WITH scripts/lib/refuse-if-running.sh. This is a deliberate,
# separate implementation, not an oversight: a PowerShell host cannot source
# a bash script, and there is no single file that runs on both. The two
# files must implement the SAME contract:
#   - refuse when a live process is executing the install target
#   - refuse (not "assume not running") when detection is inconclusive
#   - ALLOW_RUNNING_INSTALL=1 (read as an env var here too, for parity) is
#     the one documented override
# If you change the detection logic, behavior, or messaging in one file,
# make the equivalent change in the other. There is no CI check that can
# verify two files in different languages implement the same contract in
# lockstep — this comment IS the mechanism. See
# cog://mem/working/2026-07-30-self-review-spike/RETRO-486.md for why this
# repo has an explicit "two-file, not one" policy for the guard rather than
# pretending the bash file covers Windows too.
#
# ---- The fail-closed contract (matches the bash file) ----
# Any branch that cannot positively resolve a live process as "not this
# target" refuses the install. In practice, PowerShell's Get-CimInstance
# path is the single detection mechanism here (there is no procfs/lsof
# fallback chain to fall through on this platform) — so the CATCH block
# below refuses on any query failure, rather than treating an exception as
# "must not be running."
#
# Usage (dot-source, then call):
#   . .\refuse-if-running.ps1
#   Assert-CogosNotRunning -Target "$InstallDir\cogos.exe"
#
# On refusal this THROWS a terminating error -- it deliberately does NOT
# call `exit`. This function is dot-sourced directly into whatever session
# calls it, including an interactive PowerShell host when a user pastes a
# guarded install block from README.md/docs/RELEASING.md. `exit` in that
# context closes the user's PowerShell window, which is the exact
# Class-D regression this repo hit on PR #486 (a paste-into-terminal
# install snippet that killed the reader's shell on its OWN success path —
# see RETRO-486.md). `throw` stops the calling script block with a visible
# error and leaves an interactive host alive; a real automated caller
# (e.g. a CI script) can wrap the call in try/catch if it wants a specific
# exit code instead.
#
# When downloaded standalone via Invoke-WebRequest (no local checkout to
# dot-source from), fetch this file first and dot-source the downloaded
# copy — see docs/RELEASING.md's Windows install section for the pattern.

function Assert-CogosNotRunning {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Target
    )

    if ($env:ALLOW_RUNNING_INSTALL -eq "1") {
        return
    }

    if (-not (Test-Path -LiteralPath $Target)) {
        # Nothing at the target path yet -- nothing to clobber.
        return
    }

    try {
        $procs = Get-CimInstance Win32_Process -Filter "Name='cogos.exe'" -ErrorAction Stop
    } catch {
        # Query failed -- this is the inconclusive case. Per the fail-closed
        # contract, inconclusive means refuse, not "assume nothing is
        # running."
        throw "REFUSING TO INSTALL: could not query running processes (Get-CimInstance failed: $($_.Exception.Message)). Installing blind could overwrite a live production binary. Options: install to a different directory, or set `$env:ALLOW_RUNNING_INSTALL = '1'` to override."
    }

    foreach ($proc in $procs) {
        # Two-stage detection mirrors the bash guard: a name match on
        # cogos.exe is not enough on its own (a bare `cogos.exe version` or
        # `cogos.exe --help` would also match), so also require a
        # standalone "serve" token in the full command line -- this also
        # catches the `cog` wrapper's `cogos.exe --workspace <path> serve`
        # shape, where "serve" is not adjacent to the binary name.
        $tokens = @()
        if ($proc.CommandLine) {
            $tokens = $proc.CommandLine -split '\s+'
        }
        if ($tokens -contains 'serve') {
            throw "REFUSING TO INSTALL: $Target may be in use -- PID $($proc.ProcessId) is running cogos.exe with 'serve' in its command line. Installing over a running kernel's binary replaces production in place. Options: install to a different directory, stop the daemon first, or set `$env:ALLOW_RUNNING_INSTALL = '1'` to override."
        }
    }
}
