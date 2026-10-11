// What a team records from its robot, as a list of entries. field/telemetry.go applies the same rules on the server.
//
// An entry is a topic name, or a section ending in "/" that covers every topic under it, including ones the robot
// publishes later. A "!" in front excludes instead, so a saved section can leave out a topic or a smaller section. The
// most specific entry decides: one naming the path itself, else the longest section above it.
//
// Loaded first on the admin page and the team page; demo.js, topic-tree.js and app.js use these functions.

"use strict";

const isSection = (entry) => entry.endsWith("/");
const isExclusion = (entry) => entry.startsWith("!");

// Whether entries record a topic or section path, and whether an entry names the path itself rather than a section
// above it.
function recordedBy(entries, path) {
  if (entries.includes(path)) return { recorded: true, exact: true };
  if (entries.includes(`!${path}`)) return { recorded: false, exact: true };
  let longest = 0, recorded = false;
  for (const entry of entries) {
    const section = isExclusion(entry) ? entry.slice(1) : entry;
    if (isSection(section) && path.startsWith(section) && section.length > longest) {
      longest = section.length;
      recorded = !isExclusion(entry);
    }
  }
  return { recorded, exact: false };
}

// Whether a section records everything under it, now and later: it's recorded and nothing inside it is excluded.
const isWholeSection = (entries, section) => recordedBy(entries, section).recorded
  && !entries.some((entry) => isExclusion(entry) && entry !== `!${section}` && entry.startsWith(`!${section}`));

// Removes every entry at or inside a section.
const withoutSection = (entries, section) =>
  entries.filter((entry) => !(isExclusion(entry) ? entry.slice(1) : entry).startsWith(section));

// Records a topic, or a whole section (replacing anything saved inside it). Adds an entry only if one is needed.
function recordPath(entries, path) {
  const next = isSection(path) ? withoutSection(entries, path) : entries.filter((entry) => entry !== `!${path}`);
  return recordedBy(next, path).recorded ? next : [...next, path];
}

// Stops recording a topic or a whole section. Inside a saved section, that saves an exclusion.
function stopPath(entries, path) {
  const next = isSection(path) ? withoutSection(entries, path) : entries.filter((entry) => entry !== path);
  return recordedBy(next, path).recorded ? [...next, `!${path}`] : next;
}
