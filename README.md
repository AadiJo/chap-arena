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
lists everything the robot publishes. Click + to record a topic, which moves it up to the recorded list, and - to stop
recording it. You can also type a path for a robot that isn't connected yet. Each recorded topic shows its rate and
last value. Poses, numbers, booleans, strings and their arrays decode; other
structs are listed as not supported, and are recorded as base64 if you type their path.

Record, in the header, starts a session folder in `recordings/` next to the database, with a folder per team and a CSV
per topic. It records every value the robot publishes (usually 50 Hz), not a sampled rate. Each row has
`unix_time_us`, the robot's timestamp converted to this computer's clock, and `robot_time_us`, the robot's own clock.
Poses get `x`, `y` (meters) and `rotation` (radians) columns; everything else has one `value` column, with arrays as
JSON.

The clocks are synced over NT4 every few seconds, to within a few milliseconds. To line recordings up with video,
timestamp the camera frames on this computer (or one synced to it) and measure the camera's fixed latency once, for
example by sliding the pose track against the camera track until they match.

The field switch has to let robots answer on port 5810. `switch_config.txt` includes the line; add it to the `DS-FMS`
access list on a switch set up from an older copy:

```
ip access-list extended DS-FMS
 permit tcp any eq 5810 10.0.100.0 0.0.0.255 established
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
