# GenomeHub installer (Windows).
#   irm https://raw.githubusercontent.com/luizeduardocarvalho/genomehub/main/install.ps1 | iex
# Downloads the latest release, installs to %USERPROFILE%\genomehub, adds it to PATH.
$ErrorActionPreference = "Stop"

$repo = "luizeduardocarvalho/genomehub"
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }

Write-Host "resolving latest release..."
$rel = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest"
$tag = $rel.tag_name
$ver = $tag.TrimStart("v")
$url = "https://github.com/$repo/releases/download/$tag/genomehub_${ver}_windows_$arch.zip"

$dst = "$env:USERPROFILE\genomehub"
New-Item -ItemType Directory -Force $dst | Out-Null
$zip = "$env:TEMP\genomehub.zip"

Write-Host "downloading genomehub $tag (windows/$arch)..."
Invoke-WebRequest -Uri $url -OutFile $zip
Expand-Archive -Path $zip -DestinationPath $dst -Force
Remove-Item $zip

# Add to the user PATH if not already present.
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$dst*") {
	[Environment]::SetEnvironmentVariable("Path", "$userPath;$dst", "User")
	Write-Host "added $dst to your PATH (restart the terminal to use 'genomehub' anywhere)"
}

& "$dst\genomehub.exe" version
Write-Host "done. try: genomehub --help"
