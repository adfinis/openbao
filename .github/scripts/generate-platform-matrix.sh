#!/usr/bin/env bash
# generate-platform-matrix.sh, derive CI matrix from GoReleaser configs.
# Uses yq to parse GoReleaser configs.
# Usage: ./generate-platform-matrix.sh [mandatory|pending|all]

set -euo pipefail

SCOPE=${1:-mandatory}

# Check if a combination should be ignored
should_ignore() {
  local combination=$1
  local ignore_rules=$2
  
  local goos=$(echo "$combination" | cut -d'|' -f1)
  local goarch=$(echo "$combination" | cut -d'|' -f2)
  local goarm=$(echo "$combination" | cut -d'|' -f3)
  
  while IFS= read -r rule; do
    [[ -z "$rule" ]] && continue
    
    local rule_goos=$(echo "$rule" | cut -d'|' -f1)
    local rule_goarch=$(echo "$rule" | cut -d'|' -f2)
    local rule_goarm=$(echo "$rule" | cut -d'|' -f3)
    
    # Check if this combination matches the ignore rule
    local matches=true
    
    # Check goos match
    if [[ -n "$rule_goos" && "$rule_goos" != "$goos" ]]; then
      matches=false
    fi
    
    # Check goarch match
    if [[ -n "$rule_goarch" && "$rule_goarch" != "$goarch" ]]; then
      matches=false
    fi
    
    # Check goarm match
    if [[ -n "$rule_goarm" && "$rule_goarm" != "$goarm" ]]; then
      matches=false
    fi
    
    if [[ "$matches" == "true" ]]; then
      return 0  # Should be ignored
    fi
  done <<< "$ignore_rules"
  
  return 1  # Should not be ignored
}

# Generate valid combinations from a single GoReleaser config
generate_valid_combinations() {
  local file=$1
  
  # Generate all combinations
  local all_combinations=$(yq -r '.builds[] | (.goos[]?) as $goos | (.goarch[]?) as $goarch | 
    if $goarch == "arm" then 
      (.goarm[]? // "") as $goarm | "\($goos)|\($goarch)|\($goarm)"
    else 
      "\($goos)|\($goarch)|"
    end' "$file" 2>/dev/null || true)
  
  # Extract ignore rules from the same file
  local ignore_rules=$(yq -r '.builds[]?.ignore[]? | "\(.goos // "")|\(.goarch // "")|\(.goarm // "")"' "$file" 2>/dev/null || true)
  
  # Filter out ignored combinations
  local valid_combinations=""
  while IFS= read -r combination; do
    [[ -z "$combination" ]] && continue
    
    if ! should_ignore "$combination" "$ignore_rules"; then
      valid_combinations="$valid_combinations$combination"$'\n'
    fi
  done <<< "$all_combinations"
  
  echo "$valid_combinations"
}

if ! command -v yq >/dev/null 2>&1; then
  echo "yq is required but not installed" >&2
  exit 1
fi

# Generate valid combinations from both files
valid_combinations=$(generate_valid_combinations goreleaser.linux.yaml; generate_valid_combinations goreleaser.other.yaml)

# Remove duplicates and empty lines
pairs=$(echo "$valid_combinations" | grep -v '^$' | sort -u)

if [[ -z "$pairs" ]]; then
  echo "Failed to derive platform pairs from GoReleaser configs" >&2
  exit 1
fi

# Convert to JSON format
json="$(echo "$pairs" | awk -F'|' '
{
  goos=$1; arch=$2; goarm=$3;
  
  if (goos=="darwin") {
    runner="macos-latest";
    # Only include amd64 and arm64 for macOS
    if (arch!="amd64" && arch!="arm64") next;
  } else if (goos=="windows") {
    runner="windows-latest";
    # Only include amd64 and arm64 for Windows
    if (arch!="amd64" && arch!="arm64") next;
  } else if (goos=="linux") {
    runner="ubuntu-latest";
  } else {
    # Map *BSD/Illumos etc. to ubuntu runner; cross-compile only
    runner="ubuntu-latest";
  }
  
  # Determine if this platform should use Docker buildx (exotic archs we cannot compile natively)
  buildx = "false"
  if (arch == "riscv64" || arch == "s390x" || arch == "ppc64" || arch == "ppc64le") {
    buildx = "true"
  } else if (goos != "linux" && goos != "windows" && goos != "darwin") {
    buildx = "true"
  }

  printf "{\"os\":\"%s\",\"goos\":\"%s\",\"goarch\":\"%s\",\"goarm\":\"%s\",\"buildx\":%s}\n", runner, goos, arch, goarm, buildx;
}' | jq -s 'unique')"

# Partition into scopes
mandatory_jq='map(select((.goos=="linux" and .goarch=="amd64") or (.goos=="windows" and .goarch=="amd64") or (.goos=="darwin" and (.goarch=="amd64" or .goarch=="arm64"))))'
pending_jq='map(select((.goos=="linux" and .goarch=="arm64") or (.goos=="windows" and .goarch=="arm64")))'

case "$SCOPE" in
  mandatory)
    echo "$json" | jq -c "$mandatory_jq"
    ;;
  pending)
    echo "$json" | jq -c "$pending_jq"
    ;;
  all)
    echo "$json" | jq -c .
    ;;
  *)
    echo "Usage: $0 [mandatory|pending|all]" >&2
    exit 1
    ;;
esac 