// The topic tree: what's recorded from a team's robot (see field/telemetry.go and topics.js), as a tree of the topics it
// publishes with search above it. The admin page's Teams view (app.js) and the team page (team.js) both show it. A
// team's sections start open just far enough to show what it saves. A plus records a topic or a whole section and a
// minus stops it; the Recording view lists only what's recorded, so it only offers minuses. Needs /topics.js and
// /helpers.js loaded first, and /topic-tree.css for its styles.
//
// Markup: topic-search, topic-search-clear, recording-count, topic-hits (holding topic-hits-count and an add-matches
// button), topic-tree and topic-note; buttons with data-topic-view (all|recording), which get aria-pressed; and
// tree-row-template, a row of the tree with data-field name, count, type, hz and last, and [data-action=add] and
// [data-action=remove] buttons, hidden when there's nothing for them to do. Tree rows get data-kind (section, topic, or
// path for a typed path the robot doesn't publish), data-path, data-recorded, a --depth custom property and, on
// sections, aria-expanded; name gets <mark>s around search matches. Rows get data-key and are updated in place.
// topic-tree gets data-empty text for when it's empty, and data-searching while there's a search.

"use strict";

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

// A search starting with / matches the start of a path; anything else matches anywhere in it. Case doesn't matter.
function matchesSearch(path, query) {
  const lower = path.toLowerCase(), needle = query.toLowerCase();
  return query.startsWith("/") ? lower.startsWith(needle) : lower.includes(needle);
}

const byLabel = (a, b) => pathLabel(a).localeCompare(pathLabel(b), undefined, { sensitivity: "base" });

// The rows the tree shows for a team: every topic the robot publishes and every saved topic it doesn't, under nested
// sections, flattened down to the open sections (every section with a match is open while searching). Typed paths the
// robot doesn't publish get rows offering to record them anyway. Also returns how many topics match the search and the
// ones the search could add. view is "all" or "recording", and open is the team's open sections.
function treeRows(team, query, view, open) {
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

  const adding = view === "all";
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

// Wires up the tree on the page. team() returns the team to show, a field.TelemetryTeam, or null to leave the tree as
// it is; save(entries) saves that team's entries. onScreen() says whether the tree is showing, so "/" can jump to its
// search. Returns a function that renders the tree, to call whenever the team or its telemetry changes.
function setUpTopicTree({ team, save, onScreen }) {
  // Each team's open sections, by path. They start with just enough open to show each saved topic and section.
  const openSections = new Map();
  const openSectionsOf = (shown) => {
    if (!openSections.has(shown.teamId)) {
      openSections.set(shown.teamId, new Set(shown.entries.filter((entry) => !isExclusion(entry)).flatMap(sectionsAbove)));
    }
    return openSections.get(shown.teamId);
  };
  // "all" lists everything the robot publishes; "recording" lists only what's recorded.
  let topicView = "all";
  const search = $("topic-search");
  const rowsFor = (shown) => treeRows(shown, search.value.trim(), topicView, openSectionsOf(shown));

  function render() {
    const shown = team();
    if (!shown) return;
    const query = search.value.trim();
    search.placeholder = shown.connection === "connected" ? "Search topics, or type a /path" : "Type a /path to record";
    $("topic-search-clear").hidden = !search.value;
    setText($("recording-count"), String(shown.topics.length));
    for (const button of document.querySelectorAll("[data-topic-view]")) {
      button.setAttribute("aria-pressed", String(button.dataset.topicView === topicView));
    }

    const { rows, matches, addable } = rowsFor(shown);
    const tree = $("topic-tree");
    tree.toggleAttribute("data-searching", Boolean(query));
    tree.dataset.empty = shown.connection === "offField" ? "Not on the field. Type a /path above to record it anyway."
      : shown.connection === "disconnected" ? "Can't reach the robot's NetworkTables. Type a /path above to record it anyway."
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
    note.hidden = shown.connection !== "disconnected" || !shown.entries.length;
    note.textContent = "Can't reach this robot's NetworkTables, so only saved topics are listed.";
  }

  function setTopicSearch(value) {
    search.value = value;
    render();
  }

  $("topic-tree").addEventListener("click", (event) => {
    const row = event.target.closest(".trow");
    const shown = team();
    if (!row || !shown) return;
    const { path } = row.dataset;
    const action = event.target.closest("[data-action]")?.dataset.action;
    if (action === "remove") {
      save(stopPath(shown.entries, path));
    } else if (action === "add") {
      if (row.dataset.kind === "path") {
        // A typed path shows in the tree once the search clears, with the sections above it open.
        for (const section of sectionsAbove(path)) openSectionsOf(shown).add(section);
        setTopicSearch("");
      }
      save(recordPath(shown.entries, path));
    } else if (row.dataset.kind === "section" && !search.value.trim()) {
      const open = openSectionsOf(shown);
      if (!open.delete(path)) open.add(path);
      render();
    }
  });
  $("add-matches").addEventListener("click", () => {
    const shown = team();
    if (shown) save(rowsFor(shown).addable.reduce(recordPath, shown.entries));
  });
  search.addEventListener("input", render);
  search.addEventListener("keydown", (event) => { if (event.key === "Escape") setTopicSearch(""); });
  $("topic-search-clear").addEventListener("click", () => {
    setTopicSearch("");
    search.focus();
  });
  for (const button of document.querySelectorAll("[data-topic-view]")) {
    button.addEventListener("click", () => {
      topicView = button.dataset.topicView;
      render();
    });
  }
  // "/" jumps to the search, like AdvantageScope and most sites with one.
  document.addEventListener("keydown", (event) => {
    if (event.key !== "/" || !onScreen() || isTextField(event.target)) return;
    event.preventDefault();
    search.focus();
  });
  return render;
}
