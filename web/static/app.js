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
// Teams view: team-list, team-number, team-sub and teams-message, team-row-template, and the topic tree's markup (see
// topic-tree.js). team-row-template is a button with data-field station, team and rate, and gets data-alliance
// (red|blue|none) and aria-current="true" when selected. Rows get data-key and are updated in place, and team-list gets
// data-empty text for when it's empty.
//
// Software (all optional): version shows the running version. update-row (hidden unless there's an update) holds
// update-note, update-version, update-progress (a bar whose <i> width is set) and the update button. body gets
// data-update (available|none). The page reloads itself once the server comes back from an update as another version.
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
//
// Needs /topics.js, /helpers.js and /topic-tree.js loaded first.

"use strict";

const stationNames = ["R1", "R2", "R3", "B1", "B2", "B3"];
const qualityNames = ["-", "caution", "warning", "good", "excellent"];
const qualityStates = ["dim", "bad", "warn", "ok", "ok"];
const settingsFields = [
  "ApAddress", "ApPassword", "ApChannel", "SwitchAddress", "SwitchPassword", "SCCManagementEnabled",
  "RedSCCAddress", "BlueSCCAddress", "SCCUsername", "SCCPassword", "SCCStation1Port", "SCCStation2Port",
  "SCCStation3Port", "DefaultWpaKey", "RecordingFormat",
];
// Last status from the server; its assignments are what the inputs are compared against for dirty state.
let lastStatus = null;
// Last settings from the server. Saving starts from these, so fields the page has no input for are preserved.
let lastSettings = null;
// Key the server uses for an assigned station whose key is left blank (see field.Apply). Shown as the placeholder.
let defaultWpaKey = "";
const rows = [];

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

// Compares what applying would actually configure (blank key means the default) with what's applied, in the last status
// or the one given.
function isDirty(i, status = lastStatus) {
  const applied = status?.stations[i].assignment ?? { teamId: 0, wpaKey: "" };
  const current = inputAssignment(i);
  const wpaKey = current.teamId && !current.wpaKey ? defaultWpaKey : current.wpaKey;
  return current.teamId !== applied.teamId || wpaKey !== applied.wpaKey;
}

// Fills the inputs from the last status. Given the status they were compared against before, it skips stations with
// unapplied edits, so a change made elsewhere (another admin page, or a team on its own page) shows up here rather than
// being undone by the next Apply.
function loadInputsFromStatus(previous = null) {
  lastStatus.stations.forEach((station, i) => {
    if (previous && isDirty(i, previous)) return;
    const { team, wpaKey } = rows[i];
    // Setting a value, even to the same one, can move the cursor of someone about to type.
    if (team.value !== String(station.assignment.teamId || "")) team.value = station.assignment.teamId || "";
    if (wpaKey.value !== station.assignment.wpaKey) wpaKey.value = station.assignment.wpaKey;
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
  renderUpdate();
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
    const previous = lastStatus;
    lastStatus = await api("GET", "/api/status");
    loadInputsFromStatus(previous);
    renderStatus();
    if (document.body.dataset.view === "teams") await refreshTelemetry();
  } catch (error) {
    setText($("ap-status"), "app unreachable", "bad");
  } finally {
    setTimeout(poll, 1000);
  }
}

// Updates.

// Version this page was loaded from. When the status reports another, the server restarted into an update and the page
// reloads to pick up the new UI.
let pageVersion = null;

function renderUpdate() {
  const update = lastStatus.update;
  pageVersion ??= update.current;
  if (update.current !== pageVersion) {
    location.reload();
    return;
  }
  setText($("version"), update.current);
  const release = update.available;
  document.body.dataset.update = release ? "available" : "none";
  const row = $("update-row");
  if (!row) return;
  row.hidden = !release;
  if (!release) return;

  // Mirrors web.updateBlocker, which has the final say.
  const blocked = lastStatus.driverStationMode !== "off" ? "Turn FMS off to update"
    : lastStatus.recording.active ? "Stop recording to update" : "";
  const busy = update.state === "downloading" || update.state === "restarting";
  const megabytes = (bytes) => (bytes / 1e6).toFixed(1);
  const note = $("update-note");
  if (update.state === "downloading") {
    setText(note, `Downloading ${megabytes(update.downloaded)} of ${megabytes(release.size)} MB`);
  } else if (update.state === "restarting") {
    setText(note, "Restarting. This page reloads when it's back.");
  } else if (blocked) {
    setText(note, blocked, "warn");
  } else if (update.state === "failed") {
    setText(note, update.error, "bad");
  } else {
    const published = new Date(release.publishedAt).toLocaleDateString(undefined, { month: "short", day: "numeric" });
    setText(note, `Released ${published}`);
  }
  setText($("update-version"), release.version);
  const progress = $("update-progress");
  if (progress) {
    progress.hidden = update.state !== "downloading";
    progress.firstElementChild.style.width = `${Math.min(100, (100 * update.downloaded) / (release.size || 1))}%`;
  }
  const button = $("update");
  button.hidden = busy;
  button.disabled = Boolean(blocked);
}

async function startUpdate() {
  const button = $("update");
  button.disabled = true;
  try {
    lastStatus = await api("POST", "/api/update");
    renderStatus();
  } catch (error) {
    setText($("update-note"), error.message, "bad");
    button.disabled = false;
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

// Teams view: the teams on the field and any others with something saved to record, and the selected one's topic tree
// (see topic-tree.js).

// Last response from /api/telemetry.
let lastTelemetry = null;
// Team shown on the right; falls back to the first listed team when it's no longer listed.
let selectedTeam = null;

const connectionLabels = {
  connected: ["NT connected", "ok"],
  disconnected: ["no NT", "bad"],
  offField: ["not on field", "dim"],
};

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
  renderTopicTree();
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

const renderTopicTree = setUpTopicTree({
  team: () => (selectedTeam === null ? null : selectedTelemetry()),
  save: saveTopics,
  onScreen: () => document.body.dataset.view === "teams",
});

function setUpTeamsView() {
  $("team-list").addEventListener("click", (event) => {
    const row = event.target.closest("[data-key]");
    if (!row) return;
    selectedTeam = Number(row.dataset.key);
    setText($("teams-message"), "");
    renderTelemetry();
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
$("update")?.addEventListener("click", startUpdate);
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
