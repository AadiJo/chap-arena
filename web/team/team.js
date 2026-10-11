// Page logic for the team page in team/index.html. Talks to the team page's API in team.go, which works out the
// station from this computer's address, so nothing here says which station or team it is.
//
// Setup: station-panel, shown while this computer is on a station, holds station, apply-message, the topic tree's
// markup (see topic-tree.js) and teams-message. no-station, shown otherwise, holds no-station-title and
// no-station-text. station has descendants with data-field name, team, wpaKey, radio and nt, gets data-dirty and
// data-quality (0-4), and holds the #apply button. body gets data-alliance (red|blue, removed off a station) and
// data-dirty.
//
// Downloads: sessions, filled from session-template (a group holding a .session button with data-field label and
// count, and a .files list) and file-template (a download link with data-field team, ext and size). Groups get
// data-open, and a file gets data-mine when it's this station's team's and data-alliance (red|blue) for the station its
// team was on when it was recorded, if the recorder noted it. Lists get data-empty text.
//
// Nav links carry data-nav="setup" or "downloads" and get aria-current="page" when active. body gets data-view, and
// data-enter (left|right) for the side the new view should slide in from. #reach says when the field can't be reached.
//
// Needs /topics.js, /helpers.js and /topic-tree.js loaded first.

"use strict";

const qualityNames = ["-", "caution", "warning", "good", "excellent"];
const qualityStates = ["dim", "bad", "warn", "ok", "ok"];
const connectionLabels = {
  connected: ["NT connected", "ok"],
  disconnected: ["no NT", "bad"],
  offField: ["not on field", "dim"],
};

// Last response from /api/station (see stationView in team.go); null until the first one arrives.
let lastView = null;
// The team this computer handed its station to. It stays on its old team's network, off any station, until its
// Ethernet is replugged; until it's back on a station, Setup says so.
let movedTo = 0;
// Whether an Apply is on its way to the server.
let applying = false;
// Last response from /api/recordings, the sessions open in it, and the newest one seen, which opens when it appears.
let lastListing = null;
const openSessions = new Set();
let newestSession = "";

const station = $("station");
const teamInput = fieldOf(station, "team");
const wpaKeyInput = fieldOf(station, "wpaKey");

// What Apply sends. A blank password uses the team's saved one (see field.ReassignStation).
const inputAssignment = () => ({ teamId: Number(teamInput.value) || 0, wpaKey: wpaKeyInput.value });

// Whether the inputs differ from the station in a view (the last one by default). A blank password with the same team
// keeps the station's, so it isn't a change.
function isDirty(view = lastView) {
  if (!view?.station) return false;
  const { teamId, wpaKey } = inputAssignment();
  return teamId !== view.teamId || (wpaKey !== "" && wpaKey !== view.wpaKey);
}

// Puts the station's team number and password back in the inputs. Leaves an input alone if it already matches, so the
// cursor of someone about to type doesn't move.
function loadInputs() {
  if (teamInput.value !== String(lastView.teamId)) teamInput.value = lastView.teamId;
  if (wpaKeyInput.value !== lastView.wpaKey) wpaKeyInput.value = lastView.wpaKey;
}

function renderDirty() {
  const dirty = isDirty();
  document.body.dataset.dirty = String(dirty);
  station.dataset.dirty = String(dirty);
  const sameTeam = Number(teamInput.value) === lastView?.teamId;
  teamInput.classList.toggle("dirty", !sameTeam);
  wpaKeyInput.classList.toggle("dirty", wpaKeyInput.value !== "" && wpaKeyInput.value !== (sameTeam ? lastView?.wpaKey : ""));
  wpaKeyInput.placeholder = sameTeam ? "password unchanged" : "saved or default password";
  $("apply").disabled = !dirty || applying;
}

function renderSetup() {
  const onStation = Boolean(lastView?.station);
  $("station-panel").hidden = !onStation;
  $("no-station").hidden = onStation || !lastView;
  if (!onStation) {
    delete document.body.dataset.alliance;
    if (movedTo) {
      setText($("no-station-title"), "Unplug and replug your Ethernet cable");
      setText($("no-station-text"), `The station is team ${movedTo}'s now. Replugging puts this computer on ${movedTo}'s network.`);
    } else {
      setText($("no-station-title"), "This computer isn't on a station");
      setText($("no-station-text"), "Plug into a station's Ethernet to set its team number and what's recorded. Downloads work from anywhere on the field network.");
    }
    return;
  }
  document.body.dataset.alliance = lastView.station.startsWith("R") ? "red" : "blue";
  setText(fieldOf(station, "name"), lastView.station);
  renderDirty();

  // The bars show the radio's link once the AP reports this team on the station.
  const wifi = lastView.wifi;
  const linked = wifi?.TeamId === lastView.teamId && wifi.RadioLinked;
  const quality = linked ? wifi.ConnectionQuality : 0;
  station.dataset.quality = String(quality);
  if (linked) {
    // Muted when it's fine, colored like the bars when it's not.
    const state = qualityStates[quality] ?? "dim";
    setText(fieldOf(station, "radio"), qualityNames[quality] ?? "-", state === "ok" ? undefined : state);
  } else {
    setText(fieldOf(station, "radio"), ...(wifi?.TeamId === lastView.teamId ? ["no link", "bad"] : ["configuring", "warn"]));
  }
  setText(fieldOf(station, "nt"), ...connectionLabels[lastView.telemetry?.connection ?? "offField"]);
  renderTopicTree();
}

async function apply() {
  if (!isDirty() || applying) return;
  const assignment = inputAssignment();
  const moving = assignment.teamId !== lastView.teamId;
  applying = true;
  renderDirty();
  try {
    lastView = await api("PUT", "/api/station", assignment);
    if (moving && !lastView.station) {
      movedTo = assignment.teamId;
      setText($("apply-message"), "");
    } else {
      loadInputs();
      setText($("apply-message"), "Applied. Your robot's radio needs the same password to link.", "ok");
    }
  } catch (error) {
    setText($("apply-message"), error.message, "bad");
  } finally {
    applying = false;
    renderSetup();
  }
}

// Saves this team's entries and shows the result.
async function saveTopics(entries) {
  try {
    lastView = await api("PUT", "/api/topics", { topics: entries });
    setText($("teams-message"), "");
  } catch (error) {
    setText($("teams-message"), error.message, "bad");
  }
  renderSetup();
}

const renderTopicTree = setUpTopicTree({
  team: () => lastView?.telemetry ?? null,
  save: saveTopics,
  onScreen: () => document.body.dataset.view === "setup" && Boolean(lastView?.station),
});

// Downloads.

async function refreshRecordings() {
  lastListing = await api("GET", "/api/recordings");
  renderDownloads();
}

function renderDownloads() {
  if (!lastListing) return;
  const sessions = lastListing.sessions;
  if (sessions[0] && sessions[0].name !== newestSession) {
    newestSession = sessions[0].name;
    openSessions.add(newestSession);
  }
  const list = $("sessions");
  list.dataset.empty = "No logs yet. They show up here once recording starts.";
  syncChildren(list, sessions, (session) => session.name, "session-template", (group, session) => {
    group.dataset.open = String(openSessions.has(session.name));
    setText(fieldOf(group, "label"), session.label);
    setText(fieldOf(group, "count"), session.files.length ? plural(session.files.length, "robot") : "nothing recorded");
    const files = group.querySelector(".files");
    files.dataset.empty = "Nothing recorded in this session yet.";
    syncChildren(files, session.files, (file) => file.name, "file-template", (link, file) => {
      link.href = `/recordings/${encodeURIComponent(session.name)}/${encodeURIComponent(file.name)}`;
      const dot = file.name.lastIndexOf(".");
      setText(fieldOf(link, "team"), dot > 0 ? file.name.slice(0, dot) : file.name);
      setText(fieldOf(link, "ext"), dot > 0 ? file.name.slice(dot) : "");
      setText(fieldOf(link, "size"), file.size);
      link.toggleAttribute("data-mine", Boolean(lastView?.station) && file.teamId === lastView.teamId);
      // The alliance the team played on in that session, which needn't be the one it's on now.
      const alliance = file.station.startsWith("R") ? "red" : file.station.startsWith("B") ? "blue" : "";
      if (alliance) link.dataset.alliance = alliance;
      else delete link.dataset.alliance;
    });
  });
}

async function poll() {
  try {
    const previous = lastView;
    lastView = await api("GET", "/api/station");
    setText($("reach"), "");
    if (lastView.station) {
      movedTo = 0;
      // Follows the server unless there are unapplied edits, so a change made from the admin page shows up here.
      if (!isDirty(previous) || previous.teamId !== lastView.teamId) loadInputs();
    }
    renderSetup();
    if (document.body.dataset.view === "downloads") await refreshRecordings();
  } catch {
    setText($("reach"), "Can't reach the field", "bad");
  } finally {
    setTimeout(poll, 1000);
  }
}

const views = ["setup", "downloads"];

function route() {
  const view = views.find((name) => location.hash === `#${name}`) ?? "setup";
  const previous = document.body.dataset.view;
  // Slide in from the side the new tab is on, relative to the old one.
  document.body.dataset.enter = previous && views.indexOf(view) > views.indexOf(previous) ? "right" : "left";
  for (const name of views) $(`${name}-view`).hidden = view !== name;
  document.body.dataset.view = view;
  for (const link of document.querySelectorAll("[data-nav]")) {
    if (link.dataset.nav === view) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
  if (view === "downloads") refreshRecordings().catch(() => setText($("reach"), "Can't reach the field", "bad"));
}

// Team numbers are digits only, at most 5 (the highest valid team is 25599); anything else typed or pasted is dropped.
teamInput.addEventListener("input", () => {
  const digits = teamInput.value.replace(/\D/g, "").slice(0, 5);
  if (digits !== teamInput.value) teamInput.value = digits;
  // The station's password is its current team's. Typing another team clears it, so a blank one uses that team's saved
  // password; going back to the current team brings it back.
  if (lastView?.station) {
    const sameTeam = Number(digits) === lastView.teamId;
    if (!sameTeam && wpaKeyInput.value === lastView.wpaKey) wpaKeyInput.value = "";
    if (sameTeam && !wpaKeyInput.value) wpaKeyInput.value = lastView.wpaKey;
  }
  setText($("apply-message"), "");
  renderDirty();
});
wpaKeyInput.addEventListener("input", () => {
  setText($("apply-message"), "");
  renderDirty();
});
for (const input of [teamInput, wpaKeyInput]) {
  input.addEventListener("keydown", (event) => {
    if (event.key === "Enter") apply();
    if (event.key === "Escape" && lastView?.station) {
      loadInputs();
      renderDirty();
    }
  });
}
$("apply").addEventListener("click", apply);
$("sessions").addEventListener("click", (event) => {
  const group = event.target.closest(".session")?.closest(".group");
  if (!group) return;
  if (!openSessions.delete(group.dataset.key)) openSessions.add(group.dataset.key);
  renderDownloads();
});
window.addEventListener("hashchange", route);
route();
poll();
