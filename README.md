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

## Windows setup

The Windows release is meant to be the field PC's Chap Arena folder for good: `event.db`, the log and recordings live
in it, and in-app updates replace the exe in place. Unzip it somewhere permanent, like `C:\chap-arena`, and run
`Setup-ChapArena.cmd`. It asks before every change and is safe to run again:

1. Moves an existing Chap Arena into the folder: copies its database and recordings, keeps its flags (minus `-db`)
   and points its shortcuts at the new exe. The old folder is left alone.
1. Installs the patched radio API on a Vivid-Hosting VH-113 access point over SSH, after saving the stock one in
   `backup\`. The stock radio API reloads all of Wi-Fi for any change, which drops every robot; the patched one restarts
   only the stations whose SSID or WPA key changed and falls back to a full reload when it can't. It needs root SSH on
   the AP, and adds the PC's key there so the password is only typed once. The patch is
   `radio-api/per-station-reload.patch`, applied to [frc-radio-api](https://github.com/patfair/frc-radio-api) at the
   commit pinned in `release.yml`.
1. Watches a field test: drive a robot at R1 while R3 changes team, and it reports whether R1 ever lost its radio
   link.

`Rollback-ChapArena.cmd` starts the old Chap Arena again and puts the AP's stock radio API back. Everything the wizard
shows is saved in `logs\`.

## Stations page

Red and blue stations are side by side. Typing a team number (digits only) fills in the password from the database if
that team has one; otherwise a blank password uses the default WPA key from settings, shown greyed out. Apply sends all
six stations to the access point and saves any new passwords to the teams. In the background it reconfigures the switch,
and the SCC ports if SCC management is on, for only the stations whose team changed, so driver stations at the other
stations stay connected. The stock VH-113 firmware still restarts Wi-Fi on every station whenever it's configured, so
robots at unchanged stations reconnect anyway, unless its radio API is patched (see [Windows setup](#windows-setup)).
Edited stations turn amber until applied, and the undo button next to the warning puts everything back to what's
applied.
Radio link and connection quality come from the access point. The AP and switch icons in the header show their status,
with details on hover.

The applied stations are saved, and on restart the access point is only reconfigured if it no longer matches them, so
restarting the app doesn't kick robots off. The app can't read back what the switch has, though, so the first apply
after a restart, after a failed switch configuration or after changing the switch's address or password redoes every
station's VLAN and SCC port.

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
station. Teams can also pick their own from the [team page](#team-page-for-driver-stations).

The app connects to every robot on the field as a read-only NT4 client (`10.TE.AM.2:5810`, named `chap-arena`) and
shows everything the robot publishes as a tree, like AdvantageScope's sidebar. A green + records a topic, or a whole
section, which also picks up topics the robot publishes under it later. A red - stops recording; inside a saved section
it leaves out just that topic. Search sits above the tree, and typing a path that starts with `/` offers to record it
even if the robot isn't connected yet. Recording lists only what's recorded. Each recorded topic shows its rate and
last value.

Record, in the header, starts a session folder in `recordings/` next to the database, with one file per team named for
its number (`254.wpilog`), and a `stations.json` noting the station each team was on when its file started. It records every value the robot publishes (usually 50 Hz), not a sampled rate. Pick the
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

## Team page for driver stations

Driver stations open `http://10.0.100.5/` for their own station's page. It's a separate server on port 80 (change it
with `-team-port`), so teams can't reach the admin UI, which can enable robots and shows WPA keys and passwords. The log
shows the page's address at startup, or why it couldn't start (on Linux, port 80 needs root).

The page knows which station it's on from the computer's address: the switch gives each assigned team the
`10.TE.AM.0/24` network, so a request from `10.2.54.x` is team 254's. It has two tabs:

- **Setup** shows the station in its alliance's color with its team number, radio password, radio link and NT status.
  The password is the one the app last sent the AP for the station (the AP itself only reports a hash), so a team can
  check what its radio needs. A team can change the team number and password and hit Apply; that station changes and
  the other five stay as applied. A new team number clears the password, and leaving it blank uses that team's saved
  one, or the default WPA key. After a new team number, unplug and replug the driver station's Ethernet to join the new
  team's network. Under the station
  is the same topic tree as the Teams page, for picking what's recorded from this team's robot.
- **Downloads** lists every recording session, newest first and open. This team's file in each session is marked in the
  color of the alliance it played on then, which needn't be where it is now; sessions recorded before stations were
  noted mark it in white. Each file downloads rather than opening in the browser.

Anyone plugged into a station's port can see and change that station's password, so the port is the only lock. A
station on the default WPA key shows the default. From anywhere else on the
field network (the admin network, `10.0.100.x`, or this computer) the page only offers Downloads. Add `?demo` to try it
without hardware, or `?demo=B2` to stand in another station.

A driver station can stay on Wi-Fi at the same time. Each team's DHCP pool gives it a route to the FMS subnet
(`10.0.100.0/24`) through the field switch, so FMS traffic goes over Ethernet and everything else over Wi-Fi. It picks
the route up when it gets its address, so unplug and replug the Ethernet after applying stations with an older version.

A team's network only reaches the FMS while that team is assigned to a station, so teams should download their logs
before the next match's stations are applied. On Windows, allow the firewall prompt the first time the app starts.

The field switch has to let driver stations reach port 80. `switch_config.txt` includes the line; add it to the
`DS-FMS` access list on a switch set up from an older copy. Don't open port 8080, the admin UI, to team networks.

```
ip access-list extended DS-FMS
 permit tcp any host 10.0.100.5 eq www
```

## Settings page

Software shows the running version. When a newer release is on GitHub, a dot appears on the Settings tab and
**Update and restart** downloads it, checks it against GitHub's sha256 digest, replaces the binary and restarts on the
same port; the page reloads itself when it's back. FMS has to be off and nothing recording, since a restart drops every
driver station. `event.db`, the log and recordings are left alone, and the previous binary is kept beside the new one
with `.old` added to its name in case you need to go back. Builds made with `go build` show as `dev` and don't update.

Access point address, API password, channel and default WPA key; switch address and password; and optional SCC switch
management, with the port each driver station plugs into. Enter in any field saves. Apart from the default WPA key and
the SCC station ports, these are the same settings full Cheesy Arena stores.

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
