// Demo mode for previewing the UI without field hardware. Only active when the URL has ?demo; otherwise it does nothing.
// Load it in <head> without defer, before /app.js, so fetch is replaced before the first status poll.
//
// Replaces fetch for /api/* with an in-browser fake of web.go that never touches the server. Apply plays the same
// sequence as field.Apply: the AP goes CONFIGURING then ACTIVE, the switch configures in the background, and each
// assigned radio shows "no link" until it associates and links a few seconds later. Settings and WPA keys live in
// memory and reset on reload. Validation messages match field.validateAssignments and field.UpdateSettings, and a blank
// key on an assigned station gets the default WPA key, as in field.Apply.
//
// FMS mode follows field/driver_station.go: it starts off; turning it on has driver stations connect one by one (B2's
// never does, and R3's connects late enough to show a robot that missed Enable); Enabled enables whatever is connected;
// off drops them all. Reassigning a station drops its driver station, which reconnects if the new team has one.

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
    accessPointStatus: "ACTIVE",
    switchStatus: "ACTIVE",
    lastApplied: new Date().toISOString(),
    log: [],
  };
  let settings = {
    ApAddress: "10.0.100.2", ApPassword: "", ApChannel: 37, SwitchAddress: "10.0.100.3", SwitchPassword: "",
    SCCManagementEnabled: false, RedSCCAddress: "10.0.100.48", BlueSCCAddress: "10.0.100.49", SCCUsername: "admin",
    SCCPassword: "", SCCUpCommands: "configure terminal\ninterface range gigabitEthernet 1/2-4\nno shutdown\nexit\nexit\nexit",
    SCCDownCommands: "configure terminal\ninterface range gigabitEthernet 1/2-4\nshutdown\nexit\nexit\nexit",
    DefaultWpaKey: "",
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
    state.switchStatus = "CONFIGURING";
    log(`Applying stations ${describeTeams()}`);

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
    later(4500, () => {
      state.switchStatus = "ACTIVE";
      log(`Switch configured for stations ${describeTeams()}`);
    });
    return [200, state];
  }

  function route(method, path, body) {
    if (method === "GET" && path === "/api/status") return [200, state];
    if (method === "PUT" && path === "/api/stations") return apply(body);
    if (method === "PUT" && path === "/api/driver-stations") return setDriverStationMode(body.mode);
    if (method === "GET" && path === "/api/settings") return [200, settings];
    if (method === "PUT" && path === "/api/settings") {
      const key = body.DefaultWpaKey ?? "";
      if (key && (key.length < 8 || key.length > 63)) return [400, { error: "default password must be 8 to 63 characters" }];
      settings = body;
      log("Network settings saved.");
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
