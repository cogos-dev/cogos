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
#
# ---- Known divergence from the bash file (documented, not a fail-open) ----
# The bash guard has a second stage the PowerShell side does not: it
# resolves the candidate PID's executable and compares it against $Target,
# so a `cogos serve` process running a DIFFERENT install is allowed through
# (see refuse-if-running.sh's rtarget/exe comparison). This file has no
# equivalent -- it refuses for any cogos.exe process with 'serve' in its
# command line, regardless of which path that process is executing. That
# is a precision gap (it can over-refuse an install to an unrelated
# directory), not a fail-open one -- the unsafe direction this file's
# contract exists to prevent is under-refusing, and this errs the other
# way. Left as-is rather than papering over it with an untested path
# comparison; noted here so the divergence is a documented, chosen gap
# instead of a silently-discovered one.

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
        # A cogos.exe process whose CommandLine we cannot read (commonly a
        # process owned by another account/service context, which
        # Get-CimInstance returns with CommandLine as $null/empty rather
        # than throwing) is the SAME inconclusive case the bash guard hits
        # when neither /proc/$pid/cmdline nor `ps -o args=` can be read.
        # Treating "couldn't read it" as "doesn't contain serve" would be
        # exactly the fail-open this file's header calls out: refuse
        # instead of silently moving on to the next process.
        if ([string]::IsNullOrWhiteSpace($proc.CommandLine)) {
            throw "REFUSING TO INSTALL: cannot read the command line of PID $($proc.ProcessId), which is running cogos.exe. It may be running with 'serve' in a command line this process lacks permission to see. Refusing rather than guessing. Options: install to a different directory, or set `$env:ALLOW_RUNNING_INSTALL = '1'` to override."
        }

        # Two-stage detection mirrors the bash guard: a name match on
        # cogos.exe is not enough on its own (a bare `cogos.exe version` or
        # `cogos.exe --help` would also match), so also require a
        # standalone "serve" token in the full command line -- this also
        # catches the `cog` wrapper's `cogos.exe --workspace <path> serve`
        # shape, where "serve" is not adjacent to the binary name.
        $tokens = $proc.CommandLine -split '\s+'
        if ($tokens -contains 'serve') {
            throw "REFUSING TO INSTALL: $Target may be in use -- PID $($proc.ProcessId) is running cogos.exe with 'serve' in its command line. Installing over a running kernel's binary replaces production in place. Options: install to a different directory, stop the daemon first, or set `$env:ALLOW_RUNNING_INSTALL = '1'` to override."
        }
    }
}
