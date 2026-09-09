// Copyright 2026 Advait Johari. All Rights Reserved.
//
// Polls the status endpoint to show which team radios are actually linked, and whether the last
// apply reached the hardware.

const applyStatus = document.getElementById("applyStatus");

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
    if (!status.radioEnabled) {
      cell.textContent = "radio off";
      cell.className = "link unlinked";
    } else if (station.linked) {
      cell.textContent = `${station.teamId} linked, SNR ${station.signalNoiseRatio}`;
      cell.className = "link linked";
    } else {
      cell.textContent = "—";
      cell.className = "link unlinked";
    }
  });

  const errors = [status.apply.radioError, status.apply.switchError].filter(Boolean);
  if (status.apply.inProgress) {
    applyStatus.textContent = "applying...";
    applyStatus.className = "";
  } else if (errors.length > 0) {
    applyStatus.textContent = errors.join(" / ");
    applyStatus.className = "error";
  } else {
    applyStatus.textContent = `radio ${status.radio.toLowerCase()}, switch ${status.switch.toLowerCase()}`;
    applyStatus.className = "hint";
  }
};

refresh();
setInterval(refresh, 2000);
