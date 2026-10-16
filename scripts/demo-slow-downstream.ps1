param(
    [ValidateSet('start','baseline','slow','recover','verify','agent','cycle','stop')]
    [string]$Action = 'start',
    [string]$Config = '',
    [ValidateRange(1,600)][int]$Seconds = 120,
    [switch]$Agent
)
$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$demoArguments = @('scripts/observability_demo.py', $Action, '--seconds', $Seconds)
if ($Config) { $demoArguments += @('--config', $Config) }
if ($Agent) { $demoArguments += '--agent' }
$env:PYTHONUTF8 = '1'
python @demoArguments
exit $LASTEXITCODE
