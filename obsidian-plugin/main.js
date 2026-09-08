"use strict";

/*
 * TN Dashboard — thin Obsidian wrapper around the daemon's own embedded
 * React app (SPEC-ui-v2.md). Supersedes the v1 plugin, which reimplemented
 * the whole view in vanilla JS — the daemon now owns the UI (GET /ui, a
 * real SPA embedded via Go's `embed`) and this plugin does exactly two
 * things: show it in an iframe, and give it the one thing an iframe can't
 * do on its own — open a vault note — via postMessage.
 */

const { Plugin, ItemView, PluginSettingTab, Setting } = require("obsidian");

const VIEW_TYPE = "tn-dashboard";
const DEFAULT_BRIDGE_URL = "http://localhost:8391";

// THEME_VARS: the Obsidian CSS custom properties the embedded app's
// index.css remaps its own tokens onto (see webui/src/index.css's
// `:root[data-host="obsidian"]` block) — an <iframe> is a separate
// browsing context, so these values never cross into it by ordinary CSS
// inheritance; this plugin has to read them here and post them across
// explicitly (see postTheme / webui/src/lib/theme.ts on the receiving
// end). Keep this list in sync with index.css's mapping if either side
// adds a token.
const THEME_VARS = [
  "--background-primary",
  "--background-secondary",
  "--text-normal",
  "--text-muted",
  "--text-error",
  "--text-on-accent",
  "--interactive-accent",
  "--background-modifier-border",
  "--background-modifier-hover",
  "--font-interface",
];

function collectThemeVars() {
  const styles = getComputedStyle(document.body);
  const vars = {};
  for (const name of THEME_VARS) {
    const value = styles.getPropertyValue(name).trim();
    if (value) vars[name] = value;
  }
  return vars;
}

// resolveTaskInfo: api.tasks.get(path) is TaskNotes' own DOCUMENTED
// accessor (javascript-api.md: "Returns a task by its path. Returns null
// if no task is cached at the specified path." — entry point is
// `app.plugins.plugins.tasknotes.api`), so it's tried first and alone
// distinguishes two different outcomes that matter:
//   - null: the task genuinely isn't cached yet. Not an error, not
//     evidence the accessor is wrong — just fall through to openLinkText
//     (via openInTaskNotesUI returning false) same as any other miss.
//   - throws: something about this call is actually broken (wrong
//     TaskNotes version, api disabled, etc.) — fall through to the
//     unconfirmed fallback candidates below rather than trusting it.
// The fallback candidates are guesses (never confirmed against
// TaskNotes' own docs or source — see the report on the .obsidian read
// restriction that blocked reading its main.js directly) kept ONLY in
// case api.tasks.get is unavailable on an older TaskNotes version; they
// stay behind the documented accessor, not ahead of it. Tolerant of a
// wrong guess either way: a candidate that doesn't exist is just
// `undefined?.()`, and openInTaskNotesUI falls through further to
// openLinkText if nothing here pans out.
async function resolveTaskInfo(tn, path) {
  if (tn.api?.tasks?.get) {
    try {
      const info = await tn.api.tasks.get(path);
      if (info) return info;
      // Documented null case — not cached, not a bug. Deliberately does
      // NOT fall through to the guessed candidates below: if the
      // documented accessor says "not cached," treating that as
      // "wrong accessor, try something else" would be trusting an
      // unconfirmed guess over TaskNotes' own answer.
      return null;
    } catch (e) {
      console.warn("tn-dashboard: TaskNotes api.tasks.get threw, trying fallback accessors", e);
    }
  }

  const candidates = [
    () => tn.cacheManager?.getTaskInfo?.(path),
    () => tn.api?.tasks?.getByPath?.(path),
  ];
  for (const attempt of candidates) {
    try {
      const result = await attempt();
      if (result) return result;
    } catch (_e) {
      // try the next candidate
    }
  }
  return null;
}

// openInTaskNotesUI tries to open `path` in TaskNotes' own edit modal.
// Returns true only if it actually opened something; the caller is
// responsible for falling back to openLinkText on false, per
// SPEC-ui-v2.md's "must never dead-end" fallback chain (TaskNotes
// present+succeeds -> modal; missing, disabled, or throws -> plain note).
async function openInTaskNotesUI(app, path) {
  const tn = app.plugins?.plugins?.["tasknotes"];
  if (!tn || typeof tn.openTaskEditModal !== "function") return false;
  try {
    const taskInfo = await resolveTaskInfo(tn, path);
    if (!taskInfo) return false;
    tn.openTaskEditModal(taskInfo);
    return true;
  } catch (e) {
    console.warn("tn-dashboard: TaskNotes openTaskEditModal failed, falling back to the plain note", e);
    return false;
  }
}

class TnDashboardView extends ItemView {
  constructor(leaf, plugin) {
    super(leaf);
    this.plugin = plugin;
  }

  getViewType() {
    return VIEW_TYPE;
  }
  getDisplayText() {
    return "TN Dashboard";
  }
  getIcon() {
    return "layout-dashboard";
  }

  async onOpen() {
    const root = this.containerEl.children[1];
    root.empty();
    root.addClass("tn-dashboard-iframe-container");

    const bridgeUrl = (this.plugin.settings.bridgeUrl || DEFAULT_BRIDGE_URL).replace(/\/$/, "");
    const iframe = root.createEl("iframe", { cls: "tn-dashboard-iframe" });
    iframe.src = bridgeUrl + "/ui?host=obsidian";

    const postTheme = () => {
      if (iframe.contentWindow) {
        iframe.contentWindow.postMessage({ type: "obsidian-theme", vars: collectThemeVars() }, "*");
      }
    };
    iframe.addEventListener("load", postTheme);

    // Re-send on any Obsidian theme/appearance change while the view is
    // open — registerEvent ties this to the PLUGIN's lifecycle, but it's
    // harmless (a no-op postMessage into a torn-down iframe) if the view
    // itself has since closed; onClose below still removes the message
    // listener regardless.
    this.plugin.registerEvent(this.app.workspace.on("css-change", postTheme));

    // The embedded app can't call app.workspace.openLinkText itself (it's
    // sandboxed in the iframe) — it posts {type:"open-task", path}
    // instead, and this is the one place that actually performs the
    // internal navigation. Not an obsidian:// URL: openLinkText is a
    // direct workspace call, no URL-scheme round trip through the OS.
    // The SAME listener also answers "request-theme" — the app's own
    // load-order race guard for when its listener attaches after this
    // iframe's `load` event (and postTheme) already fired once.
    this.onMessage = (event) => {
      if (event.source !== iframe.contentWindow) return;
      const data = event.data;
      if (!data) return;
      if (data.type === "open-task" && typeof data.path === "string") {
        this.app.workspace.openLinkText(data.path, "", false);
      } else if (data.type === "open-task-ui" && typeof data.path === "string") {
        openInTaskNotesUI(this.app, data.path).then((opened) => {
          if (!opened) this.app.workspace.openLinkText(data.path, "", false);
        });
      } else if (data.type === "request-theme") {
        postTheme();
      }
    };
    window.addEventListener("message", this.onMessage);
  }

  async onClose() {
    if (this.onMessage) window.removeEventListener("message", this.onMessage);
  }
}

class TnDashboardSettingTab extends PluginSettingTab {
  constructor(app, plugin) {
    super(app, plugin);
    this.plugin = plugin;
  }

  display() {
    const { containerEl } = this;
    containerEl.empty();
    containerEl.createEl("h2", { text: "TN Dashboard" });

    new Setting(containerEl)
      .setName("Bridge URL")
      .setDesc("The tn serve daemon's base URL — the iframe loads <url>/ui?host=obsidian.")
      .addText((text) =>
        text
          .setPlaceholder(DEFAULT_BRIDGE_URL)
          .setValue(this.plugin.settings.bridgeUrl || "")
          .onChange(async (value) => {
            this.plugin.settings.bridgeUrl = value.trim();
            await this.plugin.saveSettings();
          })
      );
  }
}

module.exports = class TnDashboardPlugin extends Plugin {
  async onload() {
    this.settings = Object.assign({ bridgeUrl: DEFAULT_BRIDGE_URL }, await this.loadData());
    this.registerView(VIEW_TYPE, (leaf) => new TnDashboardView(leaf, this));
    this.addRibbonIcon("layout-dashboard", "Open TN Dashboard", () => this.activateView());
    this.addCommand({ id: "open-tn-dashboard", name: "TN Dashboard: open", callback: () => this.activateView() });
    this.addSettingTab(new TnDashboardSettingTab(this.app, this));
  }

  onunload() {
    this.app.workspace.detachLeavesOfType(VIEW_TYPE);
  }

  async saveSettings() {
    await this.saveData(this.settings);
  }

  async activateView() {
    const { workspace } = this.app;
    let leaf = workspace.getLeavesOfType(VIEW_TYPE)[0];
    if (!leaf) {
      leaf = workspace.getRightLeaf(false);
      await leaf.setViewState({ type: VIEW_TYPE, active: true });
    }
    workspace.revealLeaf(leaf);
  }
};
