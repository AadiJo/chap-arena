// Page logic for the UI in index.html. Talks to the JSON API in web.go.
//
// index.html supplies only markup and CSS. It must contain these ids: station-template, apply, clear,
// apply-message, log, ap-status, switch-status, stations-view, teams-view, settings-view, save-settings and
// settings-message.
// Settings inputs are optional: each settingsFields entry with a matching id is loaded and saved; the rest are kept as
// the server sent them.
//
// Optional: last-applied; linked-count; revert (a button that discards unapplied edits, hidden while there are none);
// log-dialog (a <dialog> holding #log, opened by any [data-log-open] button, closed by [data-log-close], Esc or a
// backdrop click; it gets data-closing while any exit animation the design defines plays).
//
// Nav links carry data-nav="stations", "teams" or "settings" and get aria-current="page" when active. body gets
// data-view, and data-enter (left|right) for the side the new view should slide in from.
//
// Recording: #record toggles it; #record-label shows "Record" or the elapsed time, and #record-meta shows an error if
// starting or stopping fails.
// body gets data-recording (true|false).
//
// Teams view: team-list, team-number, team-sub, topic-search, topic-search-clear, recording-count, topic-hits (holding
// topic-hits-count and an add-matches button), topic-tree, topic-note and teams-message; buttons with data-topic-view
// (all|recording), which get aria-pressed; and two templates. team-row-template is a button with data-field station, team
// and rate, and gets data-alliance (red|blue|none) and aria-current="true" when selected. tree-row-template is a row of
// the topic tree with data-field name, count, type, hz and last, and [data-action=add] and [data-action=remove] buttons,
// hidden when there's nothing for them to do. Tree rows get data-kind (section, topic, or path for a typed path the
// robot doesn't publish), data-path, data-recorded, a --depth custom property and, on sections, aria-expanded; name gets
// <mark>s around search matches. Rows get data-key and are updated in place, lists get data-empty text for when they're
// empty, and topic-tree gets data-searching while there's a search. Needs /topics.js loaded first.
//
// FMS mode: buttons with data-ds-mode (off|disabled|enabled) set it and get aria-pressed="true" when current; body gets
// data-ds-mode. Space sets disabled from anywhere but a text field while FMS is on.
//
// Stations are appended to #stations, or to [data-slot="red"] / [data-slot="blue"] when the page splits alliances.
// The station template is cloned once per station. Its root element gets data-alliance (red|blue), data-radio
// (idle|dirty|configuring|nolink|linked), data-quality (0-4), data-dirty and data-robot (enabled|disabled by FMS, or none
// while FMS is off or no robot is linked).
// Descendants with data-field set to name, team, wpaKey, radio, snr, rates or quality are filled in (snr, rates and
// quality may be left out). Optionally, a data-field="ds" container holding dsState, battery and trip shows the driver
// station; it's hidden while FMS is off or the station has no team. Text outputs get data-state (ok|warn|bad|dim) for
// styling.

"use strict";

const stationNames = ["R1", "R2", "R3", "B1", "B2", "B3"];
const qualityNames = ["-", "caution", "warning", "good", "excellent"];
const qualityStates = ["dim", "bad", "warn", "ok", "ok"];
const settingsFields = [
  "ApAddress", "ApPassword", "ApChannel", "SwitchAddress", "SwitchPassword", "SCCManagementEnabled",
  "RedSCCAddress", "BlueSCCAddress", "SCCUsername", "SCCPassword", "SCCUpCommands", "SCCDownCommands", "DefaultWpaKey",
  "RecordingFormat",
];
const $ = (id) => document.getElementById(id);

// Last status from the server; its assignments are what the inputs are compared against for dirty state.
let lastStatus = null;
// Last settings from the server. Saving starts from these, so fields the page has no input for are preserved.
let lastSettings = null;
// Key the server uses for an assigned station whose key is left blank (see field.Apply). Shown as the placeholder.
let defaultWpaKey = "";
const rows = [];

async function api(method, path, body) {
  const response = await fetch(path, {
    method,
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const json = await response.json();
  if (!response.ok) throw new Error(json.error ?? response.statusText);
  return json;
}

// Sets an output's text and its data-state styling hook (cleared when state is omitted).
function setText(element, text, state) {
  if (!element) return;
  element.textContent = text;
  if (state) element.dataset.state = state;
  else delete element.dataset.state;
}

function buildStationRows() {
  const template = $("station-template");
  stationNames.forEach((name, i) => {
    const fragment = template.content.cloneNode(true);
    const root = fragment.firstElementChild;
    const field = (key) => root.querySelector(`[data-field="${key}"]`);
    const alliance = name.startsWith("R") ? "red" : "blue";
    root.dataset.alliance = alliance;
    field("name").textContent = name;
    const row = {
      root,
      team: field("team"),
      wpaKey: field("wpaKey"),
      radio: field("radio"),
      snr: field("snr"),
      rates: field("rates"),
      quality: field("quality"),
      ds: field("ds"),
      dsState: field("dsState"),
      battery: field("battery"),
      trip: field("trip"),
    };
    rows.push(row);
    (document.querySelector(`[data-slot="${alliance}"]`) ?? $("stations")).appendChild(fragment);

    const { team, wpaKey } = row;
    // Team numbers are digits only, at most 5 (the highest valid team is 25599); anything else typed or pasted is dropped.
    // Capped here rather than with maxLength, which would cut a paste like "12ab34" before the letters are removed.
    wpaKey.dataset.placeholder = wpaKey.placeholder;
    team.addEventListener("input", () => {
      const digits = team.value.replace(/\D/g, "").slice(0, 5);
      if (digits !== team.value) team.value = digits;
      renderRow(i);
      renderDirty();
    });
    wpaKey.addEventListener("input", () => { renderRow(i); renderDirty(); });
    team.addEventListener("change", () => prefillWpaKey(i));
    // Enter applies; from the team field it waits for the password prefill first.
    team.addEventListener("keydown", async (event) => {
      if (event.key !== "Enter") return;
      await prefillWpaKey(i);
      apply();
    });
    wpaKey.addEventListener("keydown", (event) => { if (event.key === "Enter") apply(); });
  });
}

// Fills the password from event.db when a team number is entered, if that team has one stored.
async function prefillWpaKey(i) {
  const teamId = Number(rows[i].team.value);
  if (!teamId) return;
  try {
    const team = await api("GET", `/api/teams/${teamId}`);
    if (Number(rows[i].team.value) === teamId && team.wpaKey) rows[i].wpaKey.value = team.wpaKey;
  } catch {
    // Team isn't in the database yet; the typed password will be saved on apply.
  }
  renderRow(i);
  renderDirty();
}

// What gets sent on apply. A blank key is sent blank; the server substitutes the default.
function inputAssignment(i) {
  const teamId = Number(rows[i].team.value.trim()) || 0;
  return { teamId, wpaKey: teamId ? rows[i].wpaKey.value : "" };
}

// Compares what applying would actually configure (blank key means the default) with what's applied.
function isDirty(i) {
  const applied = lastStatus?.stations[i].assignment ?? { teamId: 0, wpaKey: "" };
  const current = inputAssignment(i);
  const wpaKey = current.teamId && !current.wpaKey ? defaultWpaKey : current.wpaKey;
  return current.teamId !== applied.teamId || wpaKey !== applied.wpaKey;
}

function loadInputsFromStatus() {
  lastStatus.stations.forEach((station, i) => {
    rows[i].team.value = station.assignment.teamId || "";
    rows[i].wpaKey.value = station.assignment.wpaKey;
  });
}

// Works out which radio state a station is in; see the header comment for the values.
function radioState(i) {
  const station = lastStatus?.stations[i];
  const assignment = station?.assignment ?? { teamId: 0 };
  const wifi = station?.wifi;
  if (isDirty(i)) return "dirty";
  if (!assignment.teamId) return "idle";
  if (!wifi || wifi.TeamId !== assignment.teamId) return "configuring";
  if (!wifi.RadioLinked) return "nolink";
  return "linked";
}

const radioLabels = {
  dirty: ["not applied", "dim"],
  idle: ["-", "dim"],
  configuring: ["configuring", "warn"],
  nolink: ["no link", "bad"],
  linked: ["linked", "ok"],
};

// Fills a station's driver station line from the last status.
function renderDriverStation(i) {
  const row = rows[i];
  const station = lastStatus?.stations[i];
  const mode = lastStatus?.driverStationMode ?? "off";
  const ds = station?.driverStation;
  const hasRobot = mode !== "off" && Boolean(station?.assignment.teamId && ds?.connected && ds.robotLinked);
  row.root.dataset.robot = !hasRobot ? "none" : ds.enabled ? "enabled" : "disabled";
  if (!row.ds) return;
  row.ds.hidden = mode === "off" || !station?.assignment.teamId;
  if (row.ds.hidden) return;
  if (!ds.connected) setText(row.dsState, "no DS", "dim");
  else if (!ds.robotLinked) setText(row.dsState, "no robot", "bad");
  else if (ds.enabled) setText(row.dsState, "enabled", "ok");
  // Amber when the field is enabled but this robot isn't: it connected since Enabled was pressed.
  else setText(row.dsState, "disabled", mode === "enabled" ? "warn" : undefined);
  setText(row.battery, `${ds.batteryVoltage.toFixed(1)} V`);
  row.battery.hidden = !ds.robotLinked;
  setText(row.trip, `${ds.tripTimeMs} ms`);
  row.trip.hidden = !ds.dsLinked;
}

function renderRow(i) {
  const row = rows[i];
  renderDriverStation(i);
  const state = radioState(i);
  const dirty = state === "dirty";
  row.root.dataset.radio = state;
  row.root.dataset.dirty = String(dirty);
  row.team.classList.toggle("dirty", dirty);
  row.wpaKey.classList.toggle("dirty", dirty);
  const hasTeam = Number(row.team.value) > 0;
  row.wpaKey.placeholder = hasTeam && defaultWpaKey ? defaultWpaKey : row.wpaKey.dataset.placeholder;
  setText(row.radio, ...radioLabels[state]);

  if (state !== "linked") {
    row.root.dataset.quality = "0";
    for (const element of [row.snr, row.rates, row.quality]) setText(element, "-", "dim");
    return;
  }
  const wifi = lastStatus.stations[i].wifi;
  const quality = wifi.ConnectionQuality;
  row.root.dataset.quality = String(quality);
  setText(row.snr, String(wifi.SignalNoiseRatio), wifi.SignalNoiseRatio < 25 ? "warn" : undefined);
  setText(row.rates, `${wifi.RxRate.toFixed(0)} / ${wifi.TxRate.toFixed(0)}`);
  setText(row.quality, qualityNames[quality] ?? "-", qualityStates[quality] ?? "dim");
}

function hardwareState(status) {
  if (status === "ACTIVE") return "ok";
  if (status === "ERROR") return "bad";
  if (status === "CONFIGURING") return "warn";
  return "dim";
}

function renderStatus() {
  setText($("ap-status"), lastStatus.accessPointStatus, hardwareState(lastStatus.accessPointStatus));
  setText($("switch-status"), lastStatus.switchStatus, hardwareState(lastStatus.switchStatus));
  renderRecording();
  document.body.dataset.dsMode = lastStatus.driverStationMode;
  for (const button of document.querySelectorAll("[data-ds-mode]")) {
    button.setAttribute("aria-pressed", String(button.dataset.dsMode === lastStatus.driverStationMode));
  }
  const applied = new Date(lastStatus.lastApplied);
  setText($("last-applied"), applied.getFullYear() > 1 ? `applied ${applied.toLocaleTimeString()}` : "", "dim");
  rows.forEach((_, i) => renderRow(i));

  const assigned = rows.filter((row) => row.root.dataset.radio !== "idle").length;
  const linked = rows.filter((row) => row.root.dataset.radio === "linked").length;
  setText($("linked-count"), `${linked}/${assigned} linked`, assigned && linked === assigned ? "ok" : "dim");

  renderDirty();

  const log = $("log");
  const atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 4;
  const text = lastStatus.log.join("\n");
  if (log.textContent !== text) {
    log.textContent = text;
    if (atBottom) log.scrollTop = log.scrollHeight;
  }
}

// Updates everything that depends on how many stations have unapplied edits. Runs on every keystroke as well as every
// poll, so the warning and revert button appear immediately.
function renderDirty() {
  const dirtyCount = rows.filter((_, i) => isDirty(i)).length;
  document.body.dataset.dirty = String(dirtyCount > 0);
  const revert = $("revert");
  if (revert) revert.hidden = dirtyCount === 0;
  const message = $("apply-message");
  if (!message.dataset.sticky) {
    setText(message, dirtyCount ? `${dirtyCount} unapplied change${dirtyCount === 1 ? "" : "s"}` : "", "warn");
  }
}

function showApplyMessage(text, state) {
  const message = $("apply-message");
  setText(message, text, state);
  message.dataset.sticky = "1";
  setTimeout(() => delete message.dataset.sticky, 4000);
}

async function apply() {
  const button = $("apply");
  button.disabled = true;
  try {
    const assignments = stationNames.map((_, i) => inputAssignment(i));
    lastStatus = await api("PUT", "/api/stations", assignments);
    loadInputsFromStatus();
    showApplyMessage("Applied", "ok");
  } catch (error) {
    showApplyMessage(error.message, "bad");
    lastStatus = await api("GET", "/api/status").catch(() => lastStatus);
  } finally {
    button.disabled = false;
    if (lastStatus) renderStatus();
  }
}

async function setDriverStationMode(mode) {
  try {
    lastStatus = await api("PUT", "/api/driver-stations", { mode });
    renderStatus();
  } catch (error) {
    showApplyMessage(error.message, "bad");
  }
}

async function poll() {
  try {
    const first = lastStatus === null;
    lastStatus = await api("GET", "/api/status");
    if (first) loadInputsFromStatus();
    renderStatus();
    if (document.body.dataset.view === "teams") await refreshTelemetry();
  } catch (error) {
    setText($("ap-status"), "app unreachable", "bad");
  } finally {
    setTimeout(poll, 1000);
  }
}

// Recording.

// Shows the elapsed time while recording, e.g. "3:12" or "1:03:12".
function formatElapsed(since) {
  const seconds = Math.max(0, Math.floor((Date.now() - since) / 1000));
  const pad = (n) => String(n).padStart(2, "0");
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor(seconds / 60) % 60;
  return hours ? `${hours}:${pad(minutes)}:${pad(seconds % 60)}` : `${minutes}:${pad(seconds % 60)}`;
}

function renderRecording() {
  const recording = lastStatus?.recording;
  const active = Boolean(recording?.active);
  document.body.dataset.recording = String(active);
  const button = $("record");
  if (!button) return;
  setText($("record-label"), active ? formatElapsed(new Date(recording.startedAt)) : "Record");
  button.title = active
    ? `Recording ${recording.format.toUpperCase()} to ${recording.folder}. Click to stop.`
    : "Record NetworkTables topics";
}

async function toggleRecording() {
  try {
    lastStatus = await api("PUT", "/api/recording", { active: !lastStatus?.recording?.active });
    renderStatus();
  } catch (error) {
    setText($("record-meta"), error.message, "bad");
    setTimeout(() => setText($("record-meta"), ""), 4000);
  }
}

// Teams view: what's recorded from each team's robot (see field/telemetry.go and topics.js), as a tree of the topics it
// publishes. A team's sections start open just far enough to show what it saves. A plus records a topic or a whole
// section and a minus stops it; the Recording view lists only what's recorded, so it only offers minuses.

// Last response from /api/telemetry.
let lastTelemetry = null;
// Team shown on the right; falls back to the first listed team when it's no longer listed.
let selectedTeam = null;
// Each team's open sections, by path.
const openSections = new Map();
// "all" lists everything the robot publishes; "recording" lists only what's recorded.
let topicView = "all";

const connectionLabels = {
  connected: ["NT connected", "ok"],
  disconnected: ["no NT", "bad"],
  offField: ["not on field", "dim"],
};

// Makes container's children match items, reusing each item's element by key. Rows under the pointer are updated in
// place rather than replaced, since replacing one between mousedown and mouseup would swallow the click.
function syncChildren(container, items, keyOf, templateId, update) {
  const existing = new Map([...container.children].map((element) => [element.dataset.key, element]));
  items.forEach((item, i) => {
    const key = String(keyOf(item));
    let element = existing.get(key);
    if (element) {
      existing.delete(key);
    } else {
      element = $(templateId).content.firstElementChild.cloneNode(true);
      element.dataset.key = key;
    }
    update(element, item);
    if (container.children[i] !== element) container.insertBefore(element, container.children[i] ?? null);
  });
  for (const element of existing.values()) element.remove();
}

const fieldOf = (element, key) => element.querySelector(`[data-field="${key}"]`);
const plural = (n, word, words = `${word}s`) => `${n} ${n === 1 ? word : words}`;

function selectedTelemetry() {
  return lastTelemetry?.teams.find((team) => team.teamId === selectedTeam)
    ?? { teamId: selectedTeam, station: "", connection: "offField", entries: [], topics: [], available: [] };
}

// The slowest recorded topic's rate, since that's the one to worry about. A saved section the robot publishes nothing
// under counts as no data.
function teamRate(team) {
  if (!team.entries.some((entry) => !isExclusion(entry)) || team.connection === "offField") return ["-", "dim"];
  if (team.connection === "disconnected") return ["waiting", "warn"];
  const emptySection = team.entries.some((entry) => isSection(entry) && !isExclusion(entry)
    && !team.topics.some((topic) => topic.name.startsWith(entry)));
  const rate = emptySection || !team.topics.length ? 0 : Math.min(...team.topics.map((topic) => topic.hz));
  return rate ? [`${rate} Hz`, "ok"] : ["no data", "warn"];
}

async function refreshTelemetry() {
  lastTelemetry = await api("GET", "/api/telemetry");
  renderTelemetry();
}

function renderTelemetry() {
  if (!lastTelemetry) return;
  const teams = lastTelemetry.teams;
  if (!teams.some((team) => team.teamId === selectedTeam)) selectedTeam = teams[0]?.teamId ?? null;

  const list = $("team-list");
  list.dataset.empty = "No teams on the field";
  syncChildren(list, teams, (team) => team.teamId, "team-row-template", (row, team) => {
    setText(fieldOf(row, "team"), String(team.teamId));
    setText(fieldOf(row, "station"), team.station);
    row.dataset.alliance = team.station.startsWith("R") ? "red" : team.station.startsWith("B") ? "blue" : "none";
    setText(fieldOf(row, "rate"), ...teamRate(team));
    row.setAttribute("aria-current", String(team.teamId === selectedTeam));
  });
  renderTeamDetail();
}

// The sections a topic or section path sits in, outermost first: "/A/B/c" is in "/A/" and "/A/B/".
function sectionsAbove(path) {
  const sections = [];
  for (let i = path.indexOf("/", 1); i !== -1 && i < path.length - 1; i = path.indexOf("/", i + 1)) {
    sections.push(path.slice(0, i + 1));
  }
  return sections;
}

// The last part of a topic or section path, as the tree shows it.
function pathLabel(path) {
  const trimmed = isSection(path) ? path.slice(0, -1) : path;
  return trimmed.slice(trimmed.lastIndexOf("/") + 1) || path;
}

// A team's open sections. They start with just enough open to show each saved topic and section.
function openSectionsOf(team) {
  if (!openSections.has(team.teamId)) {
    openSections.set(team.teamId, new Set(team.entries.filter((entry) => !isExclusion(entry)).flatMap(sectionsAbove)));
  }
  return openSections.get(team.teamId);
}

// A search starting with / matches the start of a path; anything else matches anywhere in it. Case doesn't matter.
function matchesSearch(path, query) {
  const lower = path.toLowerCase(), needle = query.toLowerCase();
  return query.startsWith("/") ? lower.startsWith(needle) : lower.includes(needle);
}

const byLabel = (a, b) => pathLabel(a).localeCompare(pathLabel(b), undefined, { sensitivity: "base" });

// The rows the tree shows for a team: every topic the robot publishes and every saved topic it doesn't, under nested
// sections, flattened down to the open sections (every section with a match is open while searching). Typed paths the
// robot doesn't publish get rows offering to record them anyway. Also returns how many topics match the search and the
// ones the search could add.
function treeRows(team, query) {
  const recorded = new Map(team.topics.map((topic) => [topic.name, topic]));
  const published = new Set(team.available.map((topic) => topic.name));
  const topics = [
    ...team.available.map((topic) => ({ ...topic, published: true, status: recorded.get(topic.name) })),
    ...team.topics.filter((topic) => !published.has(topic.name))
      .map((topic) => ({ ...topic, supported: true, published: false, status: topic })),
  ];
  const root = { path: "", sections: new Map(), topics: [] };
  const nodeAt = (path) => {
    let node = root;
    for (const section of [...sectionsAbove(path), ...(isSection(path) ? [path] : [])]) {
      if (!node.sections.has(section)) node.sections.set(section, { path: section, sections: new Map(), topics: [] });
      node = node.sections.get(section);
    }
    return node;
  };
  for (const topic of topics) nodeAt(topic.name).topics.push(topic);
  for (const entry of team.entries) if (isSection(entry) && !isExclusion(entry)) nodeAt(entry);

  const adding = topicView === "all";
  const showing = (topic) => (adding || topic.status) && (!query || matchesSearch(topic.name, query));
  const tally = (node) => {
    node.total = node.topics.length;
    node.recorded = node.topics.filter((topic) => topic.status).length;
    node.showing = node.topics.filter(showing).length;
    let childVisible = false;
    for (const section of node.sections.values()) {
      tally(section);
      node.total += section.total;
      node.recorded += section.recorded;
      node.showing += section.showing;
      childVisible ||= section.visible;
    }
    // A saved section the robot publishes nothing under still shows, so it can be removed.
    const emptySaved = node.total === 0 && team.entries.includes(node.path) && (!query || matchesSearch(node.path, query));
    node.visible = node.showing > 0 || childVisible || emptySaved;
  };
  tally(root);

  const live = team.connection === "connected";
  const open = openSectionsOf(team);
  const rows = [];
  if (adding && query.length > 1 && query.startsWith("/") && !root.visible) {
    const reason = live ? "This robot doesn't publish it yet." : "Can't see this robot's topics.";
    if (!isSection(query) && !recordedBy(team.entries, query).recorded) {
      rows.push({ key: `+${query}`, path: query, kind: "path", depth: 0, label: query, count: `${reason} Record it anyway.`, add: `Record ${query}` });
    }
    const section = isSection(query) ? query : `${query}/`;
    if (!isWholeSection(team.entries, section)) {
      rows.push({ key: `+${section}`, path: section, kind: "path", depth: 0, label: section, count: `Everything under ${section.slice(0, -1)}`, add: `Record everything under ${section}` });
    }
  }
  const walk = (node, depth) => {
    for (const section of [...node.sections.values()].sort((a, b) => byLabel(a.path, b.path))) {
      if (!section.visible) continue;
      const expanded = Boolean(query) || open.has(section.path);
      const label = pathLabel(section.path);
      rows.push({
        key: section.path, path: section.path, kind: "section", depth, label, expanded,
        count: section.total === 0 ? "nothing published yet"
          : section.recorded ? `${section.recorded} of ${section.total}` : plural(section.total, "topic"),
        add: adding && !isWholeSection(team.entries, section.path) && `Record all of ${label}, plus topics the robot adds to it later`,
        remove: (section.recorded > 0 || recordedBy(team.entries, section.path).recorded) && `Stop recording everything in ${label}`,
      });
      if (expanded) walk(section, depth + 1);
    }
    for (const topic of [...node.topics].sort((a, b) => byLabel(a.name, b.name))) {
      if (!showing(topic)) continue;
      const { status } = topic;
      let hz = [""], last = [""];
      if (status && live) {
        hz = [`${status.hz} Hz`, status.hz ? "ok" : "warn"];
        last = topic.published ? [status.last] : ["not published", "dim"];
      } else if (status) {
        hz = ["-", "dim"];
      } else if (!topic.supported) {
        // Only CSV recordings leave types out; a WPILOG keeps them all.
        last = ["WPILOG only", "dim"];
      }
      rows.push({
        key: topic.name, path: topic.name, kind: "topic", depth, label: pathLabel(topic.name), recorded: Boolean(status),
        type: topic.type || "-", hz, last,
        add: adding && !status && topic.supported && "Record this topic",
        remove: Boolean(status) && "Stop recording this topic",
      });
    }
  };
  walk(root, 0);
  const addable = adding ? topics.filter((topic) => showing(topic) && !topic.status && topic.supported && topic.published) : [];
  return { rows, matches: root.showing, addable: addable.map((topic) => topic.name) };
}

// Shows text with each match of query marked, ignoring case. Leaves the element alone when nothing changed, so a click
// on it isn't lost to replaced nodes.
function setHighlighted(element, text, query) {
  const needle = query && !query.includes("/") ? query.toLowerCase() : "";
  if (element.dataset.shown === `${text}\n${needle}`) return;
  element.dataset.shown = `${text}\n${needle}`;
  element.replaceChildren();
  const lower = text.toLowerCase();
  let at = 0;
  for (let i = needle ? lower.indexOf(needle) : -1; i !== -1; i = lower.indexOf(needle, at)) {
    const mark = document.createElement("mark");
    mark.textContent = text.slice(i, i + needle.length);
    element.append(text.slice(at, i), mark);
    at = i + needle.length;
  }
  element.append(text.slice(at));
}

function renderTreeRow(row, item, query) {
  row.dataset.kind = item.kind;
  row.dataset.path = item.path;
  row.style.setProperty("--depth", item.depth);
  row.title = item.kind === "path" ? "" : item.path;
  if (item.kind === "section") row.setAttribute("aria-expanded", String(item.expanded));
  else row.removeAttribute("aria-expanded");
  row.toggleAttribute("data-recorded", Boolean(item.recorded));
  setHighlighted(fieldOf(row, "name"), item.label, item.kind === "path" ? "" : query);
  setText(fieldOf(row, "count"), item.count ?? "");
  setText(fieldOf(row, "type"), item.type ?? "");
  setText(fieldOf(row, "hz"), ...(item.hz ?? [""]));
  setText(fieldOf(row, "last"), ...(item.last ?? [""]));
  for (const action of ["add", "remove"]) {
    const button = row.querySelector(`[data-action=${action}]`);
    button.hidden = !item[action];
    if (item[action]) {
      button.title = item[action];
      button.setAttribute("aria-label", item[action]);
    }
  }
}

function renderTeamDetail() {
  const detail = $("teams-view").querySelector(".team-detail");
  detail.hidden = selectedTeam === null;
  if (selectedTeam === null) return;
  const team = selectedTelemetry();
  setText($("team-number"), String(team.teamId));
  const sub = $("team-sub");
  const [connection, connectionState] = connectionLabels[team.connection];
  sub.replaceChildren();
  for (const [text, state] of [
    [connection, connectionState],
    [team.station],
  ]) {
    if (!text) continue;
    const span = document.createElement("span");
    setText(span, text, state);
    sub.append(span);
  }

  const search = $("topic-search");
  const query = search.value.trim();
  search.placeholder = team.connection === "connected" ? "Search topics, or type a /path" : "Type a /path to record";
  $("topic-search-clear").hidden = !search.value;
  setText($("recording-count"), String(team.topics.length));
  for (const button of document.querySelectorAll("[data-topic-view]")) {
    button.setAttribute("aria-pressed", String(button.dataset.topicView === topicView));
  }

  const { rows, matches, addable } = treeRows(team, query);
  const tree = $("topic-tree");
  tree.toggleAttribute("data-searching", Boolean(query));
  tree.dataset.empty = team.connection === "offField" ? "Not on the field. Type a /path above to record it anyway."
    : team.connection === "disconnected" ? "Can't reach the robot's NetworkTables. Type a /path above to record it anyway."
    : topicView === "recording" ? (query ? "Nothing recorded matches" : "Nothing recorded yet")
    : query ? "No matches" : "No topics";
  syncChildren(tree, rows, (item) => item.key, "tree-row-template", (row, item) => renderTreeRow(row, item, query));

  $("topic-hits").hidden = !query || !matches;
  setText($("topic-hits-count"), plural(matches, "match", "matches"));
  const addMatches = $("add-matches");
  addMatches.hidden = !addable.length;
  addMatches.title = addable.length === 1 ? "Record this match" : `Record all ${addable.length} matches`;
  addMatches.setAttribute("aria-label", addMatches.title);
  const note = $("topic-note");
  note.hidden = team.connection !== "disconnected" || !team.entries.length;
  note.textContent = "Can't reach this robot's NetworkTables, so only saved topics are listed.";
}

// Saves the selected team's entries and shows the result.
async function saveTopics(entries) {
  const teamId = selectedTeam;
  try {
    lastTelemetry = await api("PUT", `/api/teams/${teamId}/topics`, { topics: entries });
    setText($("teams-message"), "");
  } catch (error) {
    setText($("teams-message"), error.message, "bad");
  }
  renderTelemetry();
}

function setTopicSearch(value) {
  $("topic-search").value = value;
  renderTeamDetail();
}

function setUpTeamsView() {
  $("team-list").addEventListener("click", (event) => {
    const row = event.target.closest("[data-key]");
    if (!row) return;
    selectedTeam = Number(row.dataset.key);
    setText($("teams-message"), "");
    renderTelemetry();
  });
  $("topic-tree").addEventListener("click", (event) => {
    const row = event.target.closest(".trow");
    if (!row) return;
    const team = selectedTelemetry();
    const { path } = row.dataset;
    const action = event.target.closest("[data-action]")?.dataset.action;
    if (action === "remove") {
      saveTopics(stopPath(team.entries, path));
    } else if (action === "add") {
      if (row.dataset.kind === "path") {
        // A typed path shows in the tree once the search clears, with the sections above it open.
        for (const section of sectionsAbove(path)) openSectionsOf(team).add(section);
        setTopicSearch("");
      }
      saveTopics(recordPath(team.entries, path));
    } else if (row.dataset.kind === "section" && !$("topic-search").value.trim()) {
      const open = openSectionsOf(team);
      if (!open.delete(path)) open.add(path);
      renderTeamDetail();
    }
  });
  $("add-matches").addEventListener("click", () => {
    const team = selectedTelemetry();
    saveTopics(treeRows(team, $("topic-search").value.trim()).addable.reduce(recordPath, team.entries));
  });
  const search = $("topic-search");
  search.addEventListener("input", renderTeamDetail);
  search.addEventListener("keydown", (event) => { if (event.key === "Escape") setTopicSearch(""); });
  $("topic-search-clear").addEventListener("click", () => {
    setTopicSearch("");
    search.focus();
  });
  for (const button of document.querySelectorAll("[data-topic-view]")) {
    button.addEventListener("click", () => {
      topicView = button.dataset.topicView;
      renderTeamDetail();
    });
  }
  // "/" jumps to the search, like AdvantageScope and most sites with one.
  document.addEventListener("keydown", (event) => {
    if (event.key !== "/" || document.body.dataset.view !== "teams" || isTextField(event.target)) return;
    event.preventDefault();
    search.focus();
  });
}

// Settings view.

function buildChannelOptions(current) {
  const select = $("ApChannel");
  select.replaceChildren();
  const channels = Array.from({ length: 29 }, (_, i) => 5 + i * 8);
  if (!channels.includes(current)) channels.unshift(current);
  for (const channel of channels) select.add(new Option(String(channel), String(channel)));
  select.value = String(current);
}

// Remembers settings from the server and re-renders stations, since the default WPA key feeds their placeholders.
function useSettings(settings) {
  lastSettings = settings;
  defaultWpaKey = settings.DefaultWpaKey ?? "";
  rows.forEach((_, i) => renderRow(i));
  if (lastStatus) renderDirty();
}

async function loadSettings() {
  const settings = await api("GET", "/api/settings");
  useSettings(settings);
  if ($("ApChannel")) buildChannelOptions(settings.ApChannel);
  for (const field of settingsFields) {
    const input = $(field);
    if (!input) continue;
    if (input.type === "checkbox") input.checked = settings[field];
    else if (field !== "ApChannel") input.value = settings[field] ?? "";
  }
}

async function saveSettings() {
  const settings = { ...lastSettings };
  for (const field of settingsFields) {
    const input = $(field);
    if (!input) continue;
    settings[field] = input.type === "checkbox" ? input.checked : field === "ApChannel" ? Number(input.value) : input.value;
  }
  try {
    useSettings(await api("PUT", "/api/settings", settings));
    setText($("settings-message"), "Saved", "ok");
  } catch (error) {
    setText($("settings-message"), error.message, "bad");
  }
}

const views = ["stations", "teams", "settings"];

function route() {
  const view = views.find((name) => location.hash === `#${name}`) ?? "stations";
  const previous = document.body.dataset.view;
  // Slide in from the side the new tab is on, relative to the old one; the first view comes in from the left.
  document.body.dataset.enter = previous && views.indexOf(view) > views.indexOf(previous) ? "right" : "left";
  for (const name of views) $(`${name}-view`).hidden = view !== name;
  document.body.dataset.view = view;
  for (const link of document.querySelectorAll("[data-nav]")) {
    if (link.dataset.nav === view) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
  if (view === "settings") {
    setText($("settings-message"), "");
    loadSettings().catch((error) => setText($("settings-message"), error.message, "bad"));
  }
  if (view === "teams") refreshTelemetry().catch((error) => setText($("teams-message"), error.message, "bad"));
}

buildStationRows();
$("apply").addEventListener("click", apply);
$("clear").addEventListener("click", () => {
  for (const row of rows) row.team.value = row.wpaKey.value = "";
  if (lastStatus) renderStatus();
});
$("save-settings").addEventListener("click", saveSettings);
$("record").addEventListener("click", () => {
  // Don't leave focus on the button, where Space (which disables robots) would press it again.
  $("record").blur();
  toggleRecording();
});
setUpTeamsView();
for (const button of document.querySelectorAll("[data-ds-mode]")) {
  button.addEventListener("click", () => {
    // Don't leave focus on the button, where Space would press it again.
    button.blur();
    setDriverStationMode(button.dataset.dsMode);
  });
}
// Space disables every robot while FMS is on, except while typing in a text field. The matching keyup is swallowed too,
// since that's when a focused button would activate.
const isTextField = (target) => target.matches?.("input:not([type=checkbox]), textarea, select");
let spaceDisabled = false;
document.addEventListener("keydown", (event) => {
  if (event.key !== " " || isTextField(event.target) || (lastStatus?.driverStationMode ?? "off") === "off") return;
  event.preventDefault();
  spaceDisabled = true;
  if (!event.repeat) setDriverStationMode("disabled");
});
document.addEventListener("keyup", (event) => {
  if (event.key === " " && spaceDisabled) {
    event.preventDefault();
    spaceDisabled = false;
  }
});
// Enter in any single-line settings input saves, like Enter applies on the stations view.
$("settings-view").addEventListener("keydown", (event) => {
  if (event.key === "Enter" && event.target.matches("input:not([type=checkbox])")) saveSettings();
});
// Puts every input back to the last applied assignment.
$("revert")?.addEventListener("click", () => {
  if (!lastStatus) return;
  loadInputsFromStatus();
  renderStatus();
});
const logDialog = $("log-dialog");
for (const button of document.querySelectorAll("[data-log-open]")) {
  button.addEventListener("click", () => {
    logDialog.showModal();
    $("log").scrollTop = $("log").scrollHeight;
  });
}
// Plays the page's exit animation (if any, including on ::backdrop) before actually closing the dialog.
async function closeLogDialog() {
  if (!logDialog.open || logDialog.dataset.closing) return;
  logDialog.dataset.closing = "true";
  await Promise.all(logDialog.getAnimations({ subtree: true }).map((animation) => animation.finished.catch(() => {})));
  logDialog.close();
  delete logDialog.dataset.closing;
}
if (logDialog) {
  // A click that lands on the <dialog> itself (not its content) is a click on the backdrop.
  logDialog.addEventListener("click", (event) => { if (event.target === logDialog) closeLogDialog(); });
  // Esc fires "cancel"; take over so it animates too.
  logDialog.addEventListener("cancel", (event) => { event.preventDefault(); closeLogDialog(); });
  for (const button of logDialog.querySelectorAll("[data-log-close]")) button.addEventListener("click", closeLogDialog);
}
window.addEventListener("hashchange", route);
route();
// The stations view needs the default WPA key too, so settings load on every page, not just the settings view.
if (location.hash !== "#settings") loadSettings().catch(() => {});
poll();
