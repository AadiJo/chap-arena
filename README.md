Cheesy Arena Radio Config
=========================
A stripped-down [Cheesy Arena](https://github.com/Team254/cheesy-arena) that configures the field network. Put a
team number and radio password on each driver station, hit Apply, and the access point and switch are set up for those
teams. Nothing else about a match is run.

By default it doesn't talk to the driver stations, so teams can enable and disable their own robots as soon as their
radio links. Turn FMS on from the header when you want to enable and disable every robot at once, and use the Teams
page to record NetworkTables topics such as each robot's pose. Leave it running and change stations whenever someone
new shows up.

## Running

1. Build with `go build` (Go 1.26 or later), or use a release binary.
1. Optionally copy an `event.db` from a full Cheesy Arena install next to the binary. Its network settings and team WPA
   keys are picked up automatically. Everything else in it (matches, rankings, etc.) is left untouched, so it still
   works in full Cheesy Arena afterwards.
1. Run the binary. It opens the web UI (http://localhost:8080) in your default browser.

Flags: `-db <path>` to use a database somewhere else, `-port <n>` to change the web port, `-no-browser` to skip
opening a browser.

The log is written to the console, to `cheesy-arena.log` next to the database, and to the Log popup on the stations
page.

Add `?demo` to the URL (http://localhost:8080/?demo) to try the UI with simulated hardware. Nothing is sent to the
server in demo mode.

## Stations page

Red and blue stations are side by side. Typing a team number (digits only) fills in the password from the database if
that team has one; otherwise a blank password uses the default WPA key from settings, shown greyed out. Apply sends all
six stations to the access point, and the switch in the background, then saves any new passwords to the teams. Edited
stations turn amber until applied, and the undo button next to the warning puts everything back to what's applied.
Radio link and connection quality come from the access point. The AP and switch icons in the header show their status,
with details on hover.

The applied stations are saved, and on restart the access point is only reconfigured if it no longer matches them, so
restarting the app doesn't kick robots off.

## FMS mode

The Off / Disabled / Enabled control in the header decides who controls the robots.

- **Off** (the mode at every startup). Nothing listens for driver stations, so they stay in local mode and teams enable
  their own robots. Switching to Off from another mode closes every driver station connection, handing control back.
- **Disabled.** Driver stations connect to this app and every robot is disabled.
- **Enabled.** Every robot connected at that moment is enabled, in teleop. A robot that connects later stays disabled,
  shown in amber, until you press Enabled again.

Space switches to Disabled from anywhere on the page except while typing in a text field. While FMS is on, each station
shows whether its robot is enabled, plus battery voltage and round trip time. Applying new stations drops any driver
station whose team moved, and it reconnects to its new station.

Driver stations look for FMS at `10.0.100.5` on TCP 1750 and UDP 1160, so the computer running this app needs that
address on the field network (the log warns when it doesn't). On Windows, allow the firewall prompt the first time you
turn FMS on. If the app stops, driver stations stop hearing from it and disable their robots.

## Recording NetworkTables

The Teams page picks NetworkTables topics to record from each team's robot, for example its pose to compare against an
overhead camera. Topics are saved per team (in `event.db`, next to its WPA key), so they follow the team to any
station.

The app connects to every robot on the field as a read-only NT4 client (`10.TE.AM.2:5810`, named `chap-arena`) and
shows everything the robot publishes as a tree, like AdvantageScope's sidebar. A green + records a topic, or a whole
section, which also picks up topics the robot publishes under it later. A red - stops recording; inside a saved section
it leaves out just that topic. Search sits above the tree, and typing a path that starts with `/` offers to record it
even if the robot isn't connected yet. Recording lists only what's recorded. Each recorded topic shows its rate and
last value.

Record, in the header, starts a session folder in `recordings/` next to the database, with one file per team named for
its number (`254.wpilog`). It records every value the robot publishes (usually 50 Hz), not a sampled rate. Pick the
format under Recording on the Settings page. Both open in [AdvantageScope](https://docs.advantagescope.org/):

- **WPILOG** (the default) is WPILib's log format, as DataLogManager writes it. Every topic keeps its type, and the
  robot's struct schemas are recorded alongside, so poses go straight onto AdvantageScope's field views and other
  structs (swerve module states, `Pose3d` and so on) decode.
- **CSV** is AdvantageScope's own CSV layout (`Timestamp,Key,Value`), for line graphs, tables and spreadsheets. A pose
  is written as the fields AdvantageScope shows for one (`translation/x`, `translation/y`, `rotation/value`), so it
  graphs but can't go on a field view. Structs other than poses can't be read from a CSV, so the Teams page lists them
  as WPILOG only.

Topics are named `NT:` plus their path, the same as in a robot's own logs. Timestamps count from the start of the
recording, on this computer's clock, so every team's file in a session shares one timeline. Each file starts with a
`systemTime` value, this computer's clock in microseconds since 1970, so the wall clock time of any value is
`systemTime` plus the time from `systemTime`'s timestamp to the value's.

The clocks are synced over NT4 every few seconds, to within a few milliseconds. To line recordings up with video,
timestamp the camera frames on this computer (or one synced to it) and measure the camera's fixed latency once, for
example by sliding the pose track against the camera track until they match.

The field switch has to let robots answer on port 5810. `switch_config.txt` includes the line; add it to the `DS-FMS`
access list on a switch set up from an older copy:

```
ip access-list extended DS-FMS
 permit tcp any eq 5810 10.0.100.0 0.0.0.255 established
```

## Recordings page for driver stations

Driver stations can browse and download every recording at `http://10.0.100.5/`. It's a separate, read-only server on
port 80 (change it with `-recordings-port`), so teams can't reach the admin UI, which can enable robots and shows WPA
keys and passwords. The log shows the page's address at startup, or why it couldn't start (on Linux, port 80 needs
root).

A driver station can stay on Wi-Fi at the same time. Each team's DHCP pool gives it a route to the FMS subnet
(`10.0.100.0/24`) through the field switch, so FMS traffic goes over Ethernet and everything else over Wi-Fi. It picks
the route up when it gets its address, so unplug and replug the Ethernet after applying stations with an older version.

A team's network only reaches the FMS while that team is assigned to a station, so teams should download their logs
before the next match's stations are applied. A computer on the admin network (`10.0.100.x`) can reach the page at any
time. On Windows, allow the firewall prompt the first time the app starts.

The field switch has to let driver stations reach port 80. `switch_config.txt` includes the line; add it to the
`DS-FMS` access list on a switch set up from an older copy. Don't open port 8080, the admin UI, to team networks.

```
ip access-list extended DS-FMS
 permit tcp any host 10.0.100.5 eq www
```

## Settings page

Access point address, API password, channel and default WPA key; switch address and password; and optional SCC switch
management. Enter in any field saves. Apart from the default WPA key, these are the same settings full Cheesy Arena
stores.

See the [Advanced Networking wiki page](https://github.com/Team254/cheesy-arena/wiki/Advanced-Networking-Concepts) for
what hardware to get and how to configure it. `switch_config.txt` is the base switch configuration.

## License

Teams may use Cheesy Arena freely for practice, scrimmages, and off-season events. See [LICENSE](LICENSE) for more
details.

## Acknowledgements

[Several folks](https://github.com/Team254/cheesy-arena/graphs/contributors) have contributed pull requests. Thanks!

In addition, the following individuals have contributed to make Cheesy Arena a reality:

* Tom Bottiglieri
* James Cerar
* Kiet Chau
* Travis Covington
* Nick Eyre
* Patrick Fairbank
* Eugene Fang
* Thad House
* Ed Jordan
* Karthik Kanagasabapathy
* Ken Mitchell
* Andrew Nabors
* Jared Russell
* Ken Schenke
* Austin Schuh
* Colin Wilson
