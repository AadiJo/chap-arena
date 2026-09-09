# Chap Arena

A stripped-down fork of [Cheesy Arena](https://github.com/Team254/cheesy-arena) for at-home practice
fields. It keeps the part that is genuinely hard to reproduce, configuring the team radio and the
team ethernet switch from one place, and drops everything an offseason field doesn't need: match
play, scoring, referees, playoffs, rankings, audience displays, team signs, streams, and The Blue
Alliance publishing.

What's left is two pages.

**Stations** is the main page. Type a team number into each of the six alliance stations and press
Apply. Chap Arena configures the access point with one SSID per occupied station and the switch with
one VLAN and DHCP pool per occupied station. Leave a station blank to bypass it: no SSID is
broadcast and no VLAN is created for it, exactly as Cheesy Arena treats a bypassed station.

**Settings** holds the field hardware configuration: the access point address, password and channel,
the switch address and password, and the WPA key. Unlike Cheesy Arena, which generates a distinct
random key per team, every station here shares one key that you choose, so it can be handed out once
and left alone.

The station assignment is persisted, so restarting the server re-applies the last configuration.

## Hardware

The same hardware Cheesy Arena targets, wired the way the
[Cheesy Arena wiki](https://github.com/Team254/cheesy-arena/wiki) describes:

- A Vivid-Hosting VH-113 access point running OpenWRT, for team wifi.
- A Cisco Catalyst 3500-series switch, for team ethernet. `switch_config.txt` is the base config.

Either can be turned off independently on the settings page, so a field with only a radio works.

## Running

See `go.mod` for the Go version.

```
go build
./chap-arena
```

Then open `http://localhost:8080`. Pass `-port` to serve somewhere else.

The server reads and writes `./event.db` relative to the working directory, and loads `templates/`
and `static/` the same way, so run it from the directory holding those files.

Set an admin password on the settings page to require a login. Leaving it blank disables
authentication, which is reasonable on an isolated field network and not much else, since the page
displays the access point and switch passwords in plaintext.

## License

BSD, inherited from Cheesy Arena. See `LICENSE`. Copyright for the original work remains with
Team 254.
