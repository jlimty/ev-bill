#!/bin/sh
# Turns Trivy's JSON report into Markdown for the job summary of the CI sbom
# job. The report comes from `trivy fs --scanners vuln,license --format json`.
#
# Usage: scripts/trivy-summary.sh trivy.json > summary.md
#
# Exits non-zero, before printing anything, if the file is not a Trivy JSON
# report, so the caller can say the scan failed instead of "no findings".
set -eu

report="$1"

jq -e '.SchemaVersion == 2' "$report" > /dev/null

echo "### Vulnerabilities"
echo
vulns=$(jq '[.Results[]? | .Vulnerabilities[]?] | length' "$report")
if [ "$vulns" -eq 0 ]; then
	echo "No findings."
else
	echo "$vulns findings. Trivy matches module versions only; govulncheck (the \`vuln\` job) is the blocking gate and also checks whether the code can reach each one."
	echo
	echo "| Severity | ID | Module | Installed | Fixed in |"
	echo "| --- | --- | --- | --- | --- |"
	jq -r '
		[.Results[]? | .Vulnerabilities[]?]
		| sort_by({"CRITICAL": 0, "HIGH": 1, "MEDIUM": 2, "LOW": 3}[.Severity] // 4, .VulnerabilityID)
		| .[]
		| "| \(.Severity) | \(.VulnerabilityID) | \(.PkgName) | \(.InstalledVersion) | \(.FixedVersion // "none") |"
	' "$report"
fi

echo
echo "### Licences"
echo
licences=$(jq '[.Results[]? | .Licenses[]?] | length' "$report")
if [ "$licences" -eq 0 ]; then
	echo "No licence findings."
else
	# One count per Trivy category, e.g. "notice: 40, restricted: 1". A module
	# with two licences counts once for each.
	jq -r '
		[.Results[]? | .Licenses[]?]
		| unique_by([.PkgName, .Name])
		| group_by(.Category)
		| map("\(.[0].Category): \(length)")
		| "Module licences by category: " + join(", ") + "."
	' "$report"
	# List only the categories that may need a closer look.
	others=$(jq -r '
		[.Results[]? | .Licenses[]?]
		| unique_by([.PkgName, .Name])
		| map(select(.Category != "notice" and .Category != "permissive" and .Category != "unencumbered"))
		| sort_by(.Category, .PkgName)
		| .[]
		| "| \(.Category) | \(.Name) | \(.PkgName) |"
	' "$report")
	if [ -n "$others" ]; then
		echo
		echo "| Category | Licence | Module |"
		echo "| --- | --- | --- |"
		echo "$others"
	fi
fi
