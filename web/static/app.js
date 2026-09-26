// Page logic for the UI in index.html. Talks to the JSON API in web.go.
//
// index.html supplies only markup and CSS. It must contain these ids: station-template, apply, clear,
// apply-message, log, ap-status, switch-status, stations-view, settings-view, save-settings and settings-message.
// Settings inputs are optional: each settingsFields entry with a matching id is loaded and saved; the rest are kept as
// the server sent them.
//
// Optional: last-applied; linked-count; revert (a button that discards unapplied edits, hidden while there are none);
// log-dialog (a <dialog> holding #log, opened by any [data-log-open] button, closed by [data-log-close], Esc or a
// backdrop click; it gets data-closing while any exit animation the design defines plays).
//
// Nav links carry data-nav="stations" or data-nav="settings" and get aria-current="page" when active.
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
  } catch (error) {
    setText($("ap-status"), "app unreachable", "bad");
  } finally {
    setTimeout(poll, 1000);
  }
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

function route() {
  const view = location.hash === "#settings" ? "settings" : "stations";
  $("stations-view").hidden = view !== "stations";
  $("settings-view").hidden = view !== "settings";
  document.body.dataset.view = view;
  for (const link of document.querySelectorAll("[data-nav]")) {
    if (link.dataset.nav === view) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
  if (view === "settings") {
    setText($("settings-message"), "");
    loadSettings().catch((error) => setText($("settings-message"), error.message, "bad"));
  }
}

buildStationRows();
$("apply").addEventListener("click", apply);
$("clear").addEventListener("click", () => {
  for (const row of rows) row.team.value = row.wpaKey.value = "";
  if (lastStatus) renderStatus();
});
$("save-settings").addEventListener("click", saveSettings);
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
