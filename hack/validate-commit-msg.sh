#!/usr/bin/env bash

# Conventional Commits regex pattern
# Format: type(scope): description
# Types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert
PATTERN="^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([a-z0-9-]+\))?: .{1,100}$"

# Get the commit message
COMMIT_MSG=$(cat "$1")

# Check if the message matches the pattern
if ! echo "$COMMIT_MSG" | grep -E "$PATTERN" > /dev/null; then
    echo "Error: Commit message does not follow Conventional Commits format."
    echo "Please use the format: type(scope): description"
    echo "Types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert"
    echo "Example: feat(api): add new endpoint for user creation"
    echo "Your message: $COMMIT_MSG"
    exit 1
fi

exit 0
