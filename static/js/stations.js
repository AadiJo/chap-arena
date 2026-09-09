// Copyright 2026 Advait Johari. All Rights Reserved.
//
// Polls the status endpoint to show which team radios are actually linked, and whether the last
// apply reached the hardware.

const applyStatus = document.getElementById("applyStatus");

// Describes one station's radio state, distinguishing a deliberately bypassed station from one that
// has a team assigned but hasn't associated.
const describeStation = (station, radioEnabled) => {
  if (!radioEnabled) return ["radio off", "dimmer"];
  if (station.configuredTeamId === 0) return ["bypassed", "dimmer"];
  if (station.linked) return [`linked, SNR ${station.signalNoiseRatio}`, "ok"];
  return ["no link", "dimmer"];
};

const refresh = async () => {
  let status;
  try {
    const response = await fetch("/api/status");
    if (!response.ok) return;
    status = await response.json();
  } catch {
    return;
  }

  status.stations.forEach((station, index) => {
    const cell = document.querySelector(`.link[data-station="${index}"]`);
    if (!cell) return;
    const [text, className] = describeStation(station, status.radioEnabled);
    cell.textContent = text;
    cell.className = `link ${className}`;
  });

  const errors = [status.apply.radioError, status.apply.switchError].filter(Boolean);
  if (status.apply.inProgress) {
    applyStatus.textContent = "applying...";
    applyStatus.className = "muted";
  } else if (errors.length > 0) {
    applyStatus.textContent = errors.join(" / ");
    applyStatus.className = "error";
  } else {
    applyStatus.textContent = `radio ${status.radio.toLowerCase()}, switch ${status.switch.toLowerCase()}`;
    applyStatus.className = "muted";
  }
};

refresh();
setInterval(refresh, 2000);
