# Repository Guidelines

## Project Structure & Module Organization

`main.go` is the entry point. `field/` owns the hardware clients and applies a station assignment to
both of them. `network/` is the driver layer that talks to the access point over HTTP and the switch
over Telnet; it is inherited from Cheesy Arena and should be changed reluctantly, since it is the
only code that touches real hardware. `model/` is the BoltDB layer, holding a single settings
record. `web/` is the two-page interface, with markup in `templates/` and assets in `static/`.

This is a fork of [Cheesy Arena](https://github.com/Team254/cheesy-arena) that deliberately deleted
match play, scoring, playoffs, displays, reports, and partner integrations. Don't reintroduce them.

## Build, Test, and Development Commands

See `go.mod` for what version of Go to use.

1. `go build` builds the `chap-arena` binary in the repo root.
1. `./chap-arena` runs the server; open `http://localhost:8080`.
1. `go test -race ./...` runs all tests. Run after any change.
1. `go fmt ./...` and `go vet ./...` both need to be clean; CI enforces them.

## Coding Style & Naming Conventions

Follow standard Go style: tabs for indentation, exported names in `CamelCase`, unexported in
`camelCase`. Format with `gofmt` before submitting.

Order imports alphabetically without any grouping, empty lines, or special treatment of standard
library vs third-party imports. Don't use goimports.

## Testing Guidelines

Tests are Go `*_test.go` files co-located with packages. Use `field.SetupTestField(t)` for anything
needing a Field; it disables both pieces of hardware so tests never reach the network. Prefer
table-driven tests. Keep tests focused on behavior that could actually break.

## Interface Conventions

Dark, true black background, white primary text, information dense. No decorative card or pill
chrome, and no continuously repainting CSS animations. Minimal copy.

## Upstream

`upstream` points at Team254/cheesy-arena for pulling in fixes to `network/`. Never open a pull
request against it.
