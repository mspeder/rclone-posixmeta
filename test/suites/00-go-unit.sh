#!/usr/bin/env bash
# shellcheck shell=bash
#
# Suite 00: Go unit tests for backend/posixmeta.
#
# These tests exercise sparse-sidecar construction, inheritance, sidecar
# lifecycle, metadata filtering, and helper functions directly against the
# backend package (no CLI, no mount).

suite_require_cmd go

test_go_unit_backend() {
	local repo_root=${PM_REPO_ROOT:?PM_REPO_ROOT not exported by run.sh}
	local log="$PM_SCRATCH/go-test.log"
	(
		cd "$repo_root" || exit 1
		go test ./backend/posixmeta/ -count=1
	) 2>&1 | tee "$log"
}
