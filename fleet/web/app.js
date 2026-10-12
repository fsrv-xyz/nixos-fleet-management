function ago(timestamp) {
  const seconds = Math.max(0, Math.round((Date.now() - new Date(timestamp)) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m ago`;
}

class RelativeTime extends HTMLElement {
  static observedAttributes = ["datetime"];

  connectedCallback() {
    this.update();
    this.timer = setInterval(() => this.update(), 1000);
  }

  disconnectedCallback() {
    clearInterval(this.timer);
  }

  attributeChangedCallback() {
    this.update();
  }

  update() {
    const datetime = this.getAttribute("datetime");
    if (!datetime) return;
    this.textContent = ago(datetime);
    this.title = datetime;
  }
}

class FleetAgents extends HTMLElement {
  connectedCallback() {
    this.innerHTML = `
      <h1>Connected agents <span class="count"></span></h1>
      <p class="error" hidden></p>
      <table>
        <thead>
          <tr><th>Agent</th><th>Server</th><th>Address</th><th>Platform</th><th>Kernel</th><th>NixOS</th><th>Desired</th><th>Deployment</th><th>Connected</th></tr>
        </thead>
        <tbody></tbody>
      </table>`;
    const error = this.querySelector(".error");
    this.events = new EventSource(this.getAttribute("src"));
    this.events.onopen = () => {
      error.hidden = true;
    };
    this.events.onmessage = (event) => this.render(JSON.parse(event.data));
    this.events.onerror = () => {
      error.textContent = "Connection to server lost, reconnecting…";
      error.hidden = false;
    };
  }

  disconnectedCallback() {
    this.events.close();
  }

  render(agents) {
    this.querySelector(".count").textContent = `(${agents.length})`;
    this.querySelector("tbody").replaceChildren(...agents.map(row));
  }
}

function fact(agent, type) {
  return agent.facts?.facts?.find((f) => f["@type"] === `type.googleapis.com/${type}`);
}

const phases = {
  PHASE_REALISING: "fetching",
  PHASE_ACTIVATING: "switching",
  PHASE_DONE: "done",
  PHASE_FAILED: "failed",
};

// The hash part of a store path, enough to tell builds apart.
function short(storePath) {
  return storePath?.slice("/nix/store/".length, "/nix/store/".length + 8);
}

function cell(tr, text, title, className) {
  const td = tr.insertCell();
  td.textContent = text ?? "";
  if (title) td.title = title;
  if (className) td.className = className;
  return td;
}

function nixosCell(tr, nixos) {
  if (!nixos) return cell(tr);
  const notes = [];
  if (nixos.bootedStorePath && nixos.bootedStorePath !== nixos.storePath) notes.push("reboot pending");
  if (nixos.profileStorePath && nixos.profileStorePath !== nixos.storePath) notes.push(`next boot ${short(nixos.profileStorePath)}`);
  const text = [`#${nixos.number ?? 0} ${nixos.nixosVersion}`, ...notes].join(", ");
  return cell(tr, text, nixos.storePath, notes.length ? "warn" : "");
}

function desiredCell(tr, agent, nixos) {
  if (!agent.desired) return cell(tr, "–");
  if (agent.desired === nixos?.storePath) return cell(tr, "up to date", agent.desired, "ok");
  return cell(tr, `outdated, wants ${short(agent.desired)}`, agent.desired, "warn");
}

function deployCell(tr, deploy) {
  if (!deploy) return cell(tr);
  const td = cell(tr, `${phases[deploy.phase] ?? deploy.phase} ${short(deploy.storePath)}`, deploy.storePath,
    { PHASE_DONE: "ok", PHASE_FAILED: "fail" }[deploy.phase]);
  if (deploy.message) td.append(document.createElement("br"), deploy.message);
  const time = document.createElement("relative-time");
  time.setAttribute("datetime", deploy.updatedAt);
  td.append(" (", time, ")");
  return td;
}

function row(agent) {
  const platform = fact(agent, "fleet.v1.Platform");
  const uname = fact(agent, "fleet.v1.Uname");
  const nixos = fact(agent, "fleet.v1.NixOSGeneration");
  const tr = document.createElement("tr");
  for (const [text, title] of [
    [agent.id],
    [agent.server],
    [agent.address],
    [platform && `${platform.goos}/${platform.goarch}`],
    [uname && `${uname.sysname} ${uname.release}`,
      uname && [uname.sysname, uname.nodename, uname.release, uname.version, uname.machine].join(" ")],
  ]) {
    cell(tr, text, title);
  }
  nixosCell(tr, nixos);
  desiredCell(tr, agent, nixos);
  deployCell(tr, agent.deploy);
  const connectedAt = document.createElement("relative-time");
  connectedAt.setAttribute("datetime", agent.connectedAt);
  tr.insertCell().append(connectedAt);
  return tr;
}

customElements.define("relative-time", RelativeTime);
customElements.define("fleet-agents", FleetAgents);
