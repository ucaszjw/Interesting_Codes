#!/bin/bash
# claude-option-capture — pipe Claude Code output through this
# Detects option prompts and sends them to ClaudeBar Touch Bar
#
# Usage:
#   claude | ./claude-option-capture.sh
#   ./claude-option-capture.sh < output.log
#   echo '{"options":[{"key":"y","label":"Yes"},{"key":"n","label":"No"}]}' > ~/.claude/.touchbar_options.json

OPTIONS_FILE="$HOME/.claude/.touchbar_options.json"

write_options() {
    echo "$1" > "$OPTIONS_FILE"
}

clear_options() {
    rm -f "$OPTIONS_FILE"
}

# Detect and parse common option patterns from a line
parse_options() {
    local line="$1"

    # Pattern: (Y)es / (N)o  or  (y)es / (n)o
    if [[ "$line" =~ \(([a-zA-Z])\).*\/.*\(([a-zA-Z])\) ]]; then
        local k1="${BASH_REMATCH[1],,}" k2="${BASH_REMATCH[2],,}"
        local l1="${BASH_REMATCH[1]^}" l2="${BASH_REMATCH[2]^}"
        # Extract labels following the parenthesized letters
        local full1=$(echo "$line" | sed -n "s/.*(${BASH_REMATCH[1]})\([^(\/]*\).*/\1/p" | sed 's/^ *//;s/ *$//')
        local full2=$(echo "$line" | sed -n "s/.*\/.*(${BASH_REMATCH[2]})\s*\([^)]*\).*/\1/p" | sed 's/^ *//;s/ *$//')
        [ -n "$full1" ] && l1="$full1"
        [ -n "$full2" ] && l2="$full2"
        printf '{"options":[{"key":"%s","label":"%s"},{"key":"%s","label":"%s"}]}' "$k1" "$l1" "$k2" "$l2"
        return
    fi

    # Pattern: [Y/n] or [y/N]
    if [[ "$line" =~ \[(y|Y)\/(n|N)\] ]]; then
        local def="${BASH_REMATCH[1]}"
        if [ "$def" = "Y" ] || [ "$def" = "y" ]; then
            printf '{"options":[{"key":"y","label":"Yes"},{"key":"n","label":"No"}]}'
        else
            printf '{"options":[{"key":"n","label":"No"},{"key":"y","label":"Yes"}]}'
        fi
        return
    fi

    # Pattern: [Yes] / [No] or [Yes]/[No]
    if [[ "$line" =~ \[(Yes|No)\].*\[(No|Yes)\] ]] || [[ "$line" =~ \[(yes|no)\].*\[(no|yes)\] ]]; then
        local k1=$(echo "${BASH_REMATCH[1]}" | head -c 1 | tr '[:upper:]' '[:lower:]')
        local k2=$(echo "${BASH_REMATCH[2]}" | head -c 1 | tr '[:upper:]' '[:lower:]')
        printf '{"options":[{"key":"%s","label":"%s"},{"key":"%s","label":"%s"}]}' "$k1" "${BASH_REMATCH[1]}" "$k2" "${BASH_REMATCH[2]}"
        return
    fi

    # Pattern: [1] [2] [3] ...  numbered options
    local nums=()
    while [[ "$line" =~ \[([0-9])\].*\[([0-9])\].*\[([0-9])\] ]] || [[ "$line" =~ \[([0-9])\] ]]; then
        # Simple case: extract all [N] patterns
        for match in $(echo "$line" | grep -oE '\[[0-9]\]'); do
            local n="${match:1:1}"
            # check if already in array
            local found=0
            for existing in "${nums[@]}"; do [ "$existing" = "$n" ] && found=1; done
            [ "$found" -eq 0 ] && nums+=("$n")
        done
        break
    done

    if [ ${#nums[@]} -ge 2 ]; then
        local json='{"options":['
        local sep=""
        for n in "${nums[@]}"; do
            json+="$sep{\"key\":\"$n\",\"label\":\"$n\"}"
            sep=","
        done
        json+=']}'
        echo "$json"
        return
    fi
}

# Main: read stdin line by line
exec 3>&1  # save stdout
clear_options

while IFS= read -r line; do
    # Pass through to original stdout
    echo "$line" >&3

    # Skip empty lines
    [ -z "$line" ] && continue

    # Try to extract options
    result=$(parse_options "$line")
    if [ -n "$result" ] && [ "$result" != '{"options":[]}' ]; then
        write_options "$result"
    fi
done
