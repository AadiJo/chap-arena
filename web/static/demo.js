// Demo mode for previewing the admin UI and the team page without field hardware. Only active when the URL has ?demo;
// otherwise it does nothing. Load it in <head> without defer, before /app.js or /team.js, so fetch is replaced before
// the first poll.
//
// Replaces fetch for /api/* with an in-browser fake of web.go and team.go that never touches the server (downloads
// still go to it). Apply plays the same sequence as field.Apply: the AP goes CONFIGURING then ACTIVE, the switch
// configures the stations whose team changed in the background, and each assigned radio shows "no link" until it
// associates and links a few seconds later. Settings and WPA keys live in memory and reset on reload. Validation messages
// match field.validateAssignments and field.UpdateSettings, and a blank key on an assigned station gets the default WPA
// key, as in field.Apply.
//
// FMS mode follows field/driver_station.go: it starts off; turning it on has driver stations connect one by one (B2's
// never does, and R3's connects late enough to show a robot that missed Enable); Enabled enables whatever is connected;
// off drops them all. Reassigning a station drops its driver station, which reconnects if the new team has one.
//
// Telemetry follows field/telemetry.go: every assigned team except 971 has a robot publishing NetworkTables, from one of
// two made-up topic lists, one shaped like an AdvantageKit robot's. Teams save entries (topics, sections and exclusions,
// see topics.js); recorded topics report 50 Hz and values that change over time, formatted as the server formats them.
// Undecoded structs are supported only while the recording format is WPILOG. Recording only counts; nothing is written
// anywhere.
//
// The team page's computer stands at the station ?demo names (?demo=B2), or R1. Handing the station to another team
// takes it off any station, as unplugging would, until it "replugs" a few seconds later onto the new team's network.
// Downloads lists four made-up sessions, labeled as recordings.go labels them, with teams on different stations from one
// to the next; the oldest is from before the recorder noted stations.
//
// Updates follow the update package: v0.8.0 is offered over v0.7.1, refused while FMS is on or recording (as in
// web.updateBlocker), then downloads, restarts and comes back as v0.8.0, which reloads the page and starts over. Needs
// /topics.js loaded first.

"use strict";

(() => {
  if (!new URLSearchParams(location.search).has("demo")) return;

  const stationNames = ["R1", "R2", "R3", "B1", "B2", "B3"];
  const maxTeamId = 25599;

  // Stored WPA keys, used for prefill and updated on apply like the real team records.
  const wpaKeys = new Map([
    [254, "cheesypoofs254"], [1678, "citrus1678key"], [971, "spartan971"], [118, "robonauts118"],
    [6328, "mechadv6328"], [2056, "ofs2056key"], [1323, "madtown1323"], [4414, "hightide4414"],
  ]);
  // Link quality each station settles at once linked, so the demo shows every quality level.
  const linkProfiles = [
    { SignalNoiseRatio: 41, RxRate: 286.8, TxRate: 173.2, ConnectionQuality: 4 },
    { SignalNoiseRatio: 22, RxRate: 144.1, TxRate: 96.3, ConnectionQuality: 2 },
    { SignalNoiseRatio: 36, RxRate: 243.7, TxRate: 160.0, ConnectionQuality: 4 },
    { SignalNoiseRatio: 33, RxRate: 206.5, TxRate: 144.4, ConnectionQuality: 3 },
    { SignalNoiseRatio: 17, RxRate: 72.2, TxRate: 43.3, ConnectionQuality: 1 },
    { SignalNoiseRatio: 29, RxRate: 173.3, TxRate: 115.6, ConnectionQuality: 3 },
  ];
  // Order and delay (ms after the AP is active) in which radios link, so they don't all flip at once.
  const linkDelays = [900, 2600, 1500, 400, 3400, 2000];
  // Driver station each station gets once FMS is on: delay (ms) before it connects, battery and trip time. null never
  // connects.
  const dsProfiles = [
    { delay: 600, BatteryVoltage: 12.6, TripTimeMs: 6 },
    { delay: 1100, BatteryVoltage: 12.1, TripTimeMs: 9 },
    { delay: 7000, BatteryVoltage: 12.4, TripTimeMs: 7 },
    { delay: 1600, BatteryVoltage: 11.8, TripTimeMs: 11 },
    null,
    { delay: 900, BatteryVoltage: 12.9, TripTimeMs: 5 },
  ];
  const emptyDs = () => ({
    connected: false, enabled: false, dsLinked: false, radioLinked: false, rioLinked: false, robotLinked: false,
    batteryVoltage: 0, tripTimeMs: 0, missedPackets: 0,
  });

  const emptyWifi = () => ({ TeamId: 0, RadioLinked: false, MBits: 0, RxRate: 0, TxRate: 0, SignalNoiseRatio: 0, ConnectionQuality: 0 });
  const linkedWifi = (i, teamId) => ({ ...emptyWifi(), TeamId: teamId, RadioLinked: true, MBits: 0.4, ...linkProfiles[i] });

  const initialTeams = [254, 1678, 971, 118, 6328, 0];
  const state = {
    stations: stationNames.map((station, i) => {
      const teamId = initialTeams[i];
      return {
        station,
        assignment: { teamId, wpaKey: teamId ? wpaKeys.get(teamId) : "" },
        wifi: teamId ? linkedWifi(i, teamId) : emptyWifi(),
        driverStation: emptyDs(),
      };
    }),
    driverStationMode: "off",
    recording: { active: false, startedAt: new Date(0).toISOString(), folder: "", format: "" },
    accessPointStatus: "ACTIVE",
    switchStatus: "ACTIVE",
    lastApplied: new Date().toISOString(),
    log: [],
    update: {
      current: "v0.7.1",
      available: {
        version: "v0.8.0", size: 9_800_000,
        publishedAt: new Date(Date.now() - 2 * 86_400_000).toISOString(),
      },
      state: "idle", downloaded: 0,
    },
  };
  let settings = {
    ApAddress: "10.0.100.2", ApPassword: "", ApChannel: 37, SwitchAddress: "10.0.100.3", SwitchPassword: "",
    SCCManagementEnabled: false, RedSCCAddress: "10.0.100.48", BlueSCCAddress: "10.0.100.49", SCCUsername: "admin",
    SCCPassword: "", SCCStation1Port: "gigabitEthernet 1/2", SCCStation2Port: "gigabitEthernet 1/3",
    SCCStation3Port: "gigabitEthernet 1/4",
    DefaultWpaKey: "", RecordingFormat: "wpilog",
  };

  // Formats like Go's default logger: "2026/09/26 14:02:11 message".
  function log(message) {
    const now = new Date();
    const pad = (n) => String(n).padStart(2, "0");
    const stamp = `${now.getFullYear()}/${pad(now.getMonth() + 1)}/${pad(now.getDate())} ` +
      `${pad(now.getHours())}:${pad(now.getMinutes())}:${pad(now.getSeconds())}`;
    state.log = [...state.log, `${stamp} ${message}`].slice(-200);
  }
  const describeTeams = () =>
    state.stations.map((s) => `${s.station}=${s.assignment.teamId || "-"}`).join(" ");

  function validate(assignments) {
    const seen = new Map();
    for (const [i, { teamId, wpaKey }] of assignments.entries()) {
      const station = stationNames[i];
      if (teamId === 0) continue;
      if (teamId < 0 || teamId > maxTeamId) return `${station}: team number must be between 1 and ${maxTeamId}`;
      if (seen.has(teamId)) return `${station}: team ${teamId} is already in ${seen.get(teamId)}`;
      seen.set(teamId, station);
      if (wpaKey.length < 8 || wpaKey.length > 63) return `${station}: password must be 8 to 63 characters`;
    }
    return null;
  }

  // Each apply bumps the generation; timers from an older apply see the mismatch and do nothing, so the last apply
  // wins like the real switch mutex.
  let generation = 0;
  function later(ms, step) {
    const mine = generation;
    setTimeout(() => { if (mine === generation) step(); }, ms);
  }

  // Separate from generation: turning FMS off cancels pending driver station connections without touching the radios.
  let dsGeneration = 0;

  // Has a station's driver station connect after its delay, disabled like any new connection.
  function connectLater(i) {
    const profile = dsProfiles[i];
    const teamId = state.stations[i].assignment.teamId;
    if (!profile || !teamId || state.driverStationMode === "off") return;
    const mine = dsGeneration;
    setTimeout(() => {
      const station = state.stations[i];
      if (mine !== dsGeneration || station.assignment.teamId !== teamId || station.driverStation.connected) return;
      station.driverStation = {
        ...emptyDs(), connected: true, dsLinked: true, radioLinked: true, rioLinked: true, robotLinked: true,
        batteryVoltage: profile.BatteryVoltage, tripTimeMs: profile.TripTimeMs,
      };
      log(`Team ${teamId}'s driver station connected to ${station.station} from 10.${Math.floor(teamId / 100)}.${teamId % 100}.5.`);
    }, profile.delay);
  }

  function setDriverStationMode(mode) {
    if (!["off", "disabled", "enabled"].includes(mode)) return [400, { error: `unknown driver station mode "${mode}"` }];
    const wasOff = state.driverStationMode === "off";
    state.driverStationMode = mode;
    if (mode === "off") {
      dsGeneration++;
      state.stations.forEach((station) => { station.driverStation = emptyDs(); });
      log("FMS off; teams control their own robots.");
      return [200, state];
    }
    if (wasOff) {
      log("Listening for driver stations on TCP 1750 and UDP 1160.");
      state.stations.forEach((_, i) => connectLater(i));
    }
    state.stations.forEach((station) => {
      if (station.driverStation.connected) station.driverStation.enabled = mode === "enabled";
    });
    if (mode === "disabled") log("FMS on; all robots disabled.");
    else {
      const connected = state.stations.filter((s) => s.driverStation.connected).map((s) => `${s.station}=${s.assignment.teamId}`);
      log(`Robots enabled: ${connected.join(" ") || "none connected"}`);
    }
    return [200, state];
  }

  function apply(assignments) {
    assignments = assignments.map((a) => (a.teamId && !a.wpaKey ? { ...a, wpaKey: settings.DefaultWpaKey } : a));
    const error = validate(assignments);
    if (error) return [400, { error }];

    generation++;
    // Like the real switch, only stations whose team changed get reconfigured.
    const changed = stationNames.filter((_, i) => (assignments[i].teamId || 0) !== (state.stations[i].assignment.teamId || 0));
    assignments.forEach((assignment, i) => {
      const teamId = assignment.teamId;
      if (teamId) wpaKeys.set(teamId, assignment.wpaKey);
      if (teamId !== state.stations[i].assignment.teamId) {
        const previous = state.stations[i].driverStation;
        state.stations[i].driverStation = emptyDs();
        if (previous.connected) {
          log(`Dropping team ${state.stations[i].assignment.teamId}'s driver station from ${stationNames[i]}; the station was reassigned.`);
        }
      }
      state.stations[i].assignment = { teamId, wpaKey: teamId ? assignment.wpaKey : "" };
      state.stations[i].wifi = emptyWifi();
    });
    state.lastApplied = new Date().toISOString();
    state.accessPointStatus = "CONFIGURING";
    if (changed.length) state.switchStatus = "CONFIGURING";
    log(`Applying stations ${describeTeams()}`);
    if (!changed.length) log(`Switch already configured for stations ${describeTeams()}`);

    later(2500, () => {
      state.accessPointStatus = "ACTIVE";
      log("Access point status changed from CONFIGURING to ACTIVE.");
      state.stations.forEach((station) => {
        if (station.assignment.teamId) station.wifi = { ...emptyWifi(), TeamId: station.assignment.teamId };
      });
      state.stations.forEach((station, i) => {
        const teamId = station.assignment.teamId;
        if (teamId) later(linkDelays[i], () => { station.wifi = linkedWifi(i, teamId); });
      });
    });
    state.stations.forEach((station, i) => {
      if (!station.driverStation.connected) later(2500 + linkDelays[i], () => connectLater(i));
    });
    if (changed.length) {
      later(4500, () => {
        state.switchStatus = "ACTIVE";
        log(`Switch configured for stations ${describeTeams()} (changed ${changed.join(" ")})`);
      });
    }
    return [200, state];
  }

  // Topics each demo robot publishes, by name and NT type. The AdvantageKit list is laid out like a real AdvantageKit
  // robot's, so sections have something to hold.
  const group = (prefix, fields) => fields.map(([name, type]) => ({ name: prefix + name, type }));
  const moduleFields = [
    ["DrivePositionRad", "double"], ["DriveVelocityRadPerSec", "double"], ["DriveAppliedVolts", "double"],
    ["DriveCurrentAmps", "double"], ["TurnAbsolutePosition", "struct:Rotation2d"], ["TurnPosition", "struct:Rotation2d"],
    ["TurnVelocityRadPerSec", "double"], ["TurnAppliedVolts", "double"], ["TurnCurrentAmps", "double"],
  ];
  const motor = (unit) => [
    [`Position${unit}`, "double"], [`Velocity${unit}PerSec`, "double"], ["AppliedVolts", "double"],
    ["CurrentAmps", "double"], ["TempCelsius", "double"],
  ];
  const fmsInfo = group("/FMSInfo/", [
    ["IsRedAlliance", "boolean"], ["StationNumber", "int"], ["MatchNumber", "int"], ["EventName", "string"],
    ["GameSpecificMessage", "string"],
  ]);
  const topicLists = {
    advantageKit: [
      ...group("/AdvantageKit/DriverStation/", [
        ["Enabled", "boolean"], ["Autonomous", "boolean"], ["AllianceStation", "int"], ["MatchTime", "double"],
        ["FMSAttached", "boolean"], ["EmergencyStop", "boolean"],
      ]),
      ...group("/AdvantageKit/SystemStats/", [
        ["BatteryVoltage", "double"], ["BatteryCurrent", "double"], ["BrownedOut", "boolean"],
        ["CANBus/Utilization", "double"], ["CANBus/ReceiveErrorCount", "int"], ["CANBus/TransmitErrorCount", "int"],
      ]),
      ...group("/AdvantageKit/Drive/Gyro/", [["Connected", "boolean"], ["YawPosition", "struct:Rotation2d"]]),
      ...[0, 1, 2, 3].flatMap((i) => group(`/AdvantageKit/Drive/Module${i}/`, moduleFields)),
      ...group("/AdvantageKit/Elevator/", motor("Meters")),
      ...group("/AdvantageKit/Wrist/", motor("Rad")),
      ...["Camera0", "Camera1"].flatMap((camera) => group(`/AdvantageKit/Vision/${camera}/`, [
        ["Connected", "boolean"], ["TagIds", "int[]"], ["LatencySecs", "double"],
      ])),
      ...group("/AdvantageKit/RealOutputs/Drive/", [
        ["ModuleStates", "struct:SwerveModuleState[]"], ["ModuleSetpoints", "struct:SwerveModuleState[]"],
        ["ChassisSpeeds", "struct:ChassisSpeeds"],
      ]),
      ...group("/AdvantageKit/RealOutputs/Odometry/", [
        ["Robot", "struct:Pose2d"], ["Trajectory", "struct:Pose2d[]"], ["TrajectorySetpoint", "struct:Pose2d"],
      ]),
      ...group("/AdvantageKit/RealOutputs/Superstructure/", [
        ["Goal", "string"], ["State", "string"], ["ElevatorHeight", "double"], ["WristAngle", "double"],
      ]),
      ...group("/AdvantageKit/RealOutputs/Vision/Summary/", [["RobotPoses", "struct:Pose3d[]"], ["TagPoses", "struct:Pose3d[]"]]),
      ...group("/AdvantageKit/RealOutputs/", [["LoopTimeMs", "double"], ["MatchPhase", "string"]]),
      ...group("/SmartDashboard/", [["Field/Robot", "double[]"], ["Auto Choices/active", "string"], ["Auto Choices/options", "string[]"]]),
      ...fmsInfo,
    ],
    basic: [
      ...group("/SmartDashboard/", [
        ["Field/Robot", "double[]"], ["Gyro", "double"], ["Shooter RPM", "double"], ["Intake/HasNote", "boolean"],
        ["Auto choices/active", "string"], ["Auto choices/options", "string[]"],
      ]),
      ...fmsInfo,
    ],
  };
  const robotTopicList = { 254: "advantageKit", 6328: "advantageKit", 118: "advantageKit" };
  const unreachableRobots = new Set([971]);
  // What each team records, as saved entries (see topics.js).
  const teamEntries = new Map([
    [254, ["/AdvantageKit/RealOutputs/Odometry/", "/AdvantageKit/RealOutputs/Drive/ModuleStates", "/AdvantageKit/RealOutputs/Superstructure/State", "/AdvantageKit/SystemStats/BatteryVoltage"]],
    [1678, ["/SmartDashboard/Field/Robot"]],
    [971, ["/SmartDashboard/Field/Robot"]],
    [2056, ["/Pose"]],
  ]);
  // Types a CSV can hold (nt.Decodable); a WPILOG holds every type.
  const decodable = new Set([
    "boolean", "double", "float", "int", "string", "json", "boolean[]", "double[]", "float[]", "int[]", "string[]",
    "struct:Pose2d", "struct:Pose2d[]",
  ]);
  const pageLoaded = Date.now();

  // A topic's last value t seconds after page load, formatted as the server's formatValue would.
  function valueOf({ name, type }, t, phase) {
    const leaf = name.slice(name.lastIndexOf("/") + 1);
    const wave = (mid, amp) => mid + amp * Math.sin(t + phase + name.length);
    const a = t / 3 + phase;
    const [x, y, degrees] = [8.27 + 3 * Math.cos(a), 4.1 + 2 * Math.sin(a), (((a * 180) / Math.PI + 90) % 360) - 180];
    switch (type) {
      case "struct:Pose2d": return `${x.toFixed(2)}, ${y.toFixed(2)}, ${degrees.toFixed(1)}°`;
      case "struct:Pose2d[]": return "12 poses";
      case "double[]": return `${x.toFixed(2)}, ${y.toFixed(2)}, ${degrees.toFixed(1)}`;
      case "boolean": return String(/Connected|FMSAttached|IsRedAlliance/.test(leaf));
      case "int": return leaf === "MatchNumber" ? "42" : "0";
      case "int[]": return "4, 7";
      case "string": return leaf === "State" ? (t % 8 < 4 ? '"INTAKING"' : '"SCORING"') : leaf === "Goal" ? '"L4"' : '"Center"';
      case "string[]": return '"Left", "Center", "Right"';
      case "double":
        if (/Voltage/.test(leaf)) return wave(12.4, 0.15).toFixed(2);
        if (/Current/.test(leaf)) return Math.abs(wave(18, 14)).toFixed(2);
        return wave(0, 3).toFixed(3);
      default: return `${type.endsWith("[]") ? 64 : 24} bytes`;
    }
  }

  function telemetryTeam(teamId, station) {
    const connection = !station ? "offField" : unreachableRobots.has(teamId) ? "disconnected" : "connected";
    const published = connection === "connected" ? topicLists[robotTopicList[teamId] ?? "basic"] : [];
    const entries = teamEntries.get(teamId) ?? [];
    const supported = (type) => settings.RecordingFormat === "wpilog" || decodable.has(type);
    const t = (Date.now() - pageLoaded) / 1000;
    const phase = teamId % 7;
    const status = (name, type, live) => ({ name, type, hz: live ? 50 : 0, last: live ? valueOf({ name, type }, t, phase) : "" });
    // Saved by name first, then what a saved section picks up, as in field/telemetry.go.
    const byName = entries.filter((entry) => !isSection(entry) && !isExclusion(entry)).map((name) => {
      const topic = published.find((topic) => topic.name === name);
      return status(name, topic?.type ?? "double[]", Boolean(topic));
    });
    const picked = published.filter((topic) => {
      const { recorded, exact } = recordedBy(entries, topic.name);
      return recorded && !exact && supported(topic.type);
    }).sort((a, b) => (a.name < b.name ? -1 : 1)).map((topic) => status(topic.name, topic.type, true));
    const available = published.map(({ name, type }) => ({ name, type, supported: supported(type) }));
    return { teamId, station, connection, entries, topics: [...byName, ...picked], available };
  }

  function telemetry() {
    const teams = state.stations.filter((s) => s.assignment.teamId).map((s) => telemetryTeam(s.assignment.teamId, s.station));
    const listed = new Set(teams.map((team) => team.teamId));
    for (const teamId of [...teamEntries.keys()].sort((a, b) => a - b)) {
      if (!listed.has(teamId)) teams.push(telemetryTeam(teamId, ""));
    }
    return { teams };
  }

  // Saves entries the way field.normalizeTopics cleans them up: trimmed (after a "!" too), without blanks or duplicates.
  function setTeamTopics(teamId, body) {
    if (!(teamId >= 1 && teamId <= maxTeamId)) return [400, { error: `team number must be between 1 and ${maxTeamId}` }];
    if (!Array.isArray(body.topics)) return [400, { error: "topics must be a list" }];
    const entries = [...new Set(body.topics.map((entry) => {
      const trimmed = entry.trim();
      return isExclusion(trimmed) ? (trimmed.slice(1).trim() ? `!${trimmed.slice(1).trim()}` : "") : trimmed;
    }).filter(Boolean))];
    if (entries.length > 50) return [400, { error: "at most 50 topics per team" }];
    if (entries.length) teamEntries.set(teamId, entries);
    else teamEntries.delete(teamId);
    return [200, telemetry()];
  }

  // The team page's view, as team.go builds it for the computer at myStation.
  const myStation = Math.max(0, stationNames.indexOf((new URLSearchParams(location.search).get("demo") ?? "").toUpperCase()));
  let replugging = false;
  function stationView() {
    const { station, assignment, wifi } = state.stations[myStation];
    if (replugging || !assignment.teamId) return { station: "", teamId: 0, wpaKey: "", wifi: null, telemetry: null };
    return {
      station, teamId: assignment.teamId, wpaKey: assignment.wpaKey, wifi,
      telemetry: telemetryTeam(assignment.teamId, station),
    };
  }
  const notOnStation = [403, { error: "this computer isn't on a station's network" }];

  // Follows field.ReassignStation: a blank key uses the team's saved one, and the other stations stay as they are.
  function reassignStation(body) {
    const view = stationView();
    if (!view.station) return notOnStation;
    if (!body.teamId) return [400, { error: "enter a team number" }];
    const assignments = state.stations.map((s) => s.assignment);
    assignments[myStation] = { teamId: body.teamId, wpaKey: body.wpaKey || wpaKeys.get(body.teamId) || "" };
    const [status, result] = apply(assignments);
    if (status !== 200) return [status, result];
    log(`Team ${view.teamId} changed ${view.station} to team ${body.teamId} from the team page.`);
    if (body.teamId !== view.teamId) {
      replugging = true;
      setTimeout(() => { replugging = false; }, 6000);
    }
    return [200, stationView()];
  }

  function setStationTopics(body) {
    const view = stationView();
    if (!view.station) return notOnStation;
    const [status, result] = setTeamTopics(view.teamId, body);
    return status === 200 ? [200, stationView()] : [status, result];
  }

  // Sessions for the Downloads tab, named like recorder folders and labeled like recordings.go's sessionLabel.
  function recordings() {
    const pad = (n) => String(n).padStart(2, "0");
    // Days ago, start time, and each team's station then ("" when not noted).
    const sessions = [
      [0, 14, 32, { 254: "R1", 1678: "R2", 971: "R3", 118: "B1", 6328: "B2" }],
      [0, 11, 5, { 118: "R1", 254: "R2", 1678: "R3", 2056: "B1", 971: "B2", 6328: "B3" }],
      [1, 16, 30, { 1678: "R1", 6328: "R3", 254: "B1", 118: "B2", 2056: "B3" }],
      [8, 9, 0, { 254: "", 118: "" }],
    ];
    const today = new Date();
    return {
      sessions: sessions.map(([daysAgo, hour, minute, teams]) => {
        const at = new Date(today.getFullYear(), today.getMonth(), today.getDate() - daysAgo, hour, minute, 5);
        const name = `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}_${pad(hour)}${pad(minute)}05`;
        const clock = `${hour % 12 || 12}:${pad(minute)} ${hour < 12 ? "AM" : "PM"}`;
        const day = daysAgo === 0 ? "Today" : daysAgo === 1 ? "Yesterday"
          : at.toLocaleDateString("en-US", { month: "short", day: "numeric" });
        const files = Object.entries(teams).map(([team, station]) => ({ teamId: Number(team), station }))
          .sort((a, b) => a.teamId - b.teamId)
          .map(({ teamId, station }) => ({
            name: `${teamId}.wpilog`, teamId, station, size: `${(2 + ((teamId + hour * 7) % 19) / 10).toFixed(1)} MB`,
          }));
        return { name, label: `${day}, ${clock}`, files };
      }),
    };
  }

  function setRecording(active) {
    if (active && !state.recording.active) {
      const now = new Date();
      const pad = (n) => String(n).padStart(2, "0");
      const folder = `recordings/${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}_` +
        `${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`;
      const format = settings.RecordingFormat;
      state.recording = { ...state.recording, active: true, startedAt: now.toISOString(), folder, format };
      log(`Recording NetworkTables as ${format.toUpperCase()} to ${folder}`);
    } else if (!active && state.recording.active) {
      const topics = telemetry().teams.reduce((count, team) => count + team.topics.filter((t) => t.hz > 0).length, 0);
      const values = Math.round((Date.now() - new Date(state.recording.startedAt)) / 20) * topics;
      log(`Recorded ${values} values to ${state.recording.folder}`);
      state.recording = { ...state.recording, active: false, folder: "", format: "" };
    }
    return [200, state];
  }

  function installUpdate() {
    const update = state.update;
    if (state.driverStationMode !== "off") return [409, { error: "turn FMS off to update" }];
    if (state.recording.active) return [409, { error: "stop recording to update" }];
    if (update.state === "downloading" || update.state === "restarting") {
      return [409, { error: "an update is already in progress" }];
    }
    state.update = { ...update, state: "downloading", downloaded: 0, error: undefined };
    log(`Downloading Chap Arena ${update.available.version}`);
    const tick = setInterval(() => {
      const downloaded = Math.min(update.available.size, state.update.downloaded + 450_000);
      state.update = { ...state.update, downloaded };
      if (downloaded < update.available.size) return;
      clearInterval(tick);
      state.update = { ...state.update, state: "restarting" };
      log(`Restarting into Chap Arena ${update.available.version}`);
      setTimeout(() => {
        state.update = { current: update.available.version, available: null, state: "idle", downloaded: 0 };
      }, 2500);
    }, 200);
    return [200, state];
  }

  function route(method, path, body) {
    if (method === "POST" && path === "/api/update") return installUpdate();
    if (method === "GET" && path === "/api/status") return [200, state];
    if (method === "GET" && path === "/api/station") return [200, stationView()];
    if (method === "PUT" && path === "/api/station") return reassignStation(body);
    if (method === "PUT" && path === "/api/topics") return setStationTopics(body);
    if (method === "GET" && path === "/api/recordings") return [200, recordings()];
    if (method === "GET" && path === "/api/telemetry") return [200, telemetry()];
    if (method === "PUT" && path === "/api/recording") return setRecording(Boolean(body.active));
    const teamTopicsPath = path.match(/^\/api\/teams\/(\d+)\/topics$/);
    if (method === "PUT" && teamTopicsPath) return setTeamTopics(Number(teamTopicsPath[1]), body);
    if (method === "PUT" && path === "/api/stations") return apply(body);
    if (method === "PUT" && path === "/api/driver-stations") return setDriverStationMode(body.mode);
    if (method === "GET" && path === "/api/settings") return [200, settings];
    if (method === "PUT" && path === "/api/settings") {
      const key = body.DefaultWpaKey ?? "";
      if (key && (key.length < 8 || key.length > 63)) return [400, { error: "default password must be 8 to 63 characters" }];
      const format = body.RecordingFormat || "wpilog";
      if (!["wpilog", "csv"].includes(format)) return [400, { error: `unknown recording format "${format}"` }];
      settings = { ...body, RecordingFormat: format };
      log("Settings saved.");
      return [200, settings];
    }
    const team = path.match(/^\/api\/teams\/(\d+)$/);
    if (method === "GET" && team) {
      const teamId = Number(team[1]);
      return wpaKeys.has(teamId)
        ? [200, { teamId, wpaKey: wpaKeys.get(teamId) }]
        : [404, { error: `team ${teamId} not found` }];
    }
    return null;
  }

  const realFetch = window.fetch.bind(window);
  window.fetch = async (input, init = {}) => {
    const path = new URL(String(input), location.href).pathname;
    const method = (init.method ?? "GET").toUpperCase();
    const result = route(method, path, init.body ? JSON.parse(init.body) : undefined);
    if (!result) return realFetch(input, init);
    const [status, json] = result;
    // Small delay so buttons visibly disable, as they would against the real server.
    await new Promise((resolve) => setTimeout(resolve, 150));
    return new Response(JSON.stringify(json), { status, headers: { "Content-Type": "application/json" } });
  };
})();
