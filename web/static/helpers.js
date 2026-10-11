// Small helpers the page scripts share: app.js and topic-tree.js on the admin page, and team.js and topic-tree.js on
// the team page.

"use strict";

const $ = (id) => document.getElementById(id);

// Calls the JSON API and resolves to the response. Throws the server's error message for anything but a 2xx.
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

// Whether the target takes typing, so keyboard shortcuts leave it alone.
const isTextField = (target) => target.matches?.("input:not([type=checkbox]), textarea, select");
