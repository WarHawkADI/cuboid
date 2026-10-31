# Logs free commit memory and the largest processes every 10 seconds, so an
# out-of-memory crash on a shared laptop can be attributed afterwards.
param([string]$Out = "data/mem.log")
while ($true) {
    $os = Get-CimInstance Win32_OperatingSystem
    $top = Get-Process | Group-Object ProcessName | ForEach-Object {
        [pscustomobject]@{ Name = $_.Name; MB = [math]::Round(($_.Group | Measure-Object PagedMemorySize64 -Sum).Sum / 1MB) }
    } | Sort-Object MB -Descending | Select-Object -First 8 | ForEach-Object { "$($_.Name)=$($_.MB)" }
    "{0} commit_free_gb={1:N1} ram_free_gb={2:N1} {3}" -f (Get-Date -Format HH:mm:ss), ($os.FreeVirtualMemory / 1MB), ($os.FreePhysicalMemory / 1MB), ($top -join " ") | Out-File -Append -Encoding utf8 $Out
    Start-Sleep -Seconds 10
}
