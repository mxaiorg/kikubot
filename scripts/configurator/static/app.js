(() => {
  // ---- dirty-form tracking ----
  // Every <form> inside the main content area is wired so its primary submit
  // button starts disabled, becomes enabled on the first user-driven change,
  // and the page warns on navigation away while changes are unsaved.
  // Programmatic value changes elsewhere in this file dispatch `input`
  // events so they count as dirty too.
  const dirtyForms = new WeakSet();
  const isDirty = () => {
    let any = false;
    document.querySelectorAll("form").forEach((f) => { if (dirtyForms.has(f)) any = true; });
    return any;
  };
  document.querySelectorAll("form").forEach((form) => {
    const submit = form.querySelector('button[type="submit"], button.primary');
    if (submit) submit.disabled = true;
    const markDirty = () => {
      if (dirtyForms.has(form)) return;
      dirtyForms.add(form);
      if (submit) submit.disabled = false;
    };
    form.addEventListener("input", markDirty);
    form.addEventListener("change", markDirty);
    form.addEventListener("submit", () => {
      // Submitting is itself a navigation; clear the flag so beforeunload
      // doesn't fire after the user clicks Save.
      dirtyForms.delete(form);
    });
  });
  window.addEventListener("beforeunload", (e) => {
    if (!isDirty()) return;
    // Modern browsers ignore the custom message, but setting returnValue
    // (and calling preventDefault) is required to trigger the native
    // "leave or stay" prompt — which is what gives the user a Cancel option.
    e.preventDefault();
    e.returnValue = "";
  });

  // ---- theme toggle ----
  const root = document.documentElement;
  const stored = localStorage.getItem("ac-theme");
  if (stored === "light" || stored === "dark") root.dataset.theme = stored;
  document.getElementById("theme-toggle")?.addEventListener("click", () => {
    const next = root.dataset.theme === "dark" ? "light" : "dark";
    root.dataset.theme = next;
    localStorage.setItem("ac-theme", next);
  });

  // ---- flash auto-hide ----
  const flash = document.getElementById("flash");
  if (flash && flash.classList.contains("show")) {
    setTimeout(() => flash.classList.remove("show"), 4000);
  }

  // ---- agent form: whitelist/blacklist mutual exclusion ----
  const wl = document.getElementById("agent-whitelist");
  const bl = document.getElementById("agent-blacklist");
  const syncACL = () => {
    if (!wl || !bl) return;
    const wlSet = wl.value.trim() !== "";
    const blSet = bl.value.trim() !== "";
    bl.disabled = wlSet;
    wl.disabled = !wlSet && blSet;
  };
  wl?.addEventListener("input", syncACL);
  bl?.addEventListener("input", syncACL);
  syncACL();

  // ---- agent form: provider-dependent fields ----
  const provider = document.getElementById("agent-provider");
  const syncProvider = () => {
    if (!provider) return;
    const v = provider.value;
    document.querySelectorAll("[data-openrouter-only]").forEach((el) => {
      el.classList.toggle("is-hidden", v !== "openrouter");
    });
    document.querySelectorAll("[data-provider-hint]").forEach((el) => {
      el.classList.toggle("is-hidden", el.dataset.providerHint !== v);
    });
  };
  provider?.addEventListener("change", syncProvider);
  syncProvider();

  // ---- agent form: coordinator-prompt toggle ----
  // Toggling the checkbox seeds the System Prompt textarea, but only when
  // the textarea is empty or still equal to the *other* default — never
  // overwrite content the user has already typed.
  document.querySelectorAll("[data-coordinator-toggle]").forEach((root) => {
    const cb = root.querySelector("#coordinator-toggle");
    const ta = root.querySelector("#system-prompt-textarea");
    const sysDefault = root.dataset.systemDefault || "";
    const coordDefault = root.dataset.coordinatorDefault || "";
    if (!cb || !ta) return;
    cb.addEventListener("change", () => {
      const cur = ta.value;
      let next = cur;
      if (cb.checked) {
        if (cur.trim() === "" || cur === sysDefault) next = coordDefault;
      } else {
        if (cur.trim() === "" || cur === coordDefault) next = sysDefault;
      }
      if (next !== cur) {
        ta.value = next;
        ta.dispatchEvent(new Event("input", { bubbles: true }));
      }
    });
  });

  // ---- email service: enabled toggle ----
  const enabled = document.getElementById("email-enabled");
  const fields = document.getElementById("email-fields");
  enabled?.addEventListener("change", () => {
    if (enabled.checked) fields?.removeAttribute("disabled");
    else fields?.setAttribute("disabled", "");
  });

  // ---- email service: hostname → agent_domain default ----
  const hostname = document.getElementById("email-hostname");
  const domain = document.getElementById("email-domain");
  // Live mirror of the agent domain into any hint text marked with
  // [data-agent-domain-display]. Reflects edits as the user types.
  const domainDisplays = document.querySelectorAll("[data-agent-domain-display]");
  const syncDomainDisplays = () => {
    if (!domain) return;
    const v = domain.value.trim();
    domainDisplays.forEach((el) => { el.textContent = v; });
  };
  hostname?.addEventListener("blur", () => {
    if (!domain || domain.value.trim() !== "") return;
    const h = hostname.value.trim();
    const i = h.indexOf(".");
    if (i > 0 && i < h.length - 1) {
      domain.value = h.slice(i + 1);
      domain.dispatchEvent(new Event("input", { bubbles: true }));
      syncDomainDisplays();
    }
  });
  domain?.addEventListener("input", syncDomainDisplays);

  // ---- chip-input (multi-select with add/remove) ----
  document.querySelectorAll("[data-chip-input]").forEach((root) => {
    let tools = {};
    try { tools = JSON.parse(root.dataset.tools || "{}"); } catch (_) {}
    const allKeys = Object.keys(tools).sort();
    const hidden = root.querySelector("input[type=hidden]");
    const list = root.querySelector(".chips-list");
    const addBtn = root.querySelector(".add-chip");
    const picker = root.querySelector(".chip-picker");
    if (!hidden || !list || !addBtn || !picker) return;

    // Optional provider-aware filtering: tools that aren't compatible with
    // the currently-selected LLM provider are hidden from the picker; if
    // already-selected, the warning banner is shown.
    const providerEl = root.dataset.providerSource
      ? document.getElementById(root.dataset.providerSource)
      : null;
    const warnEl = root.querySelector("[data-tool-warning]");
    // Map of toolKey -> provider that DISALLOWS it. Extend here if other
    // tools become provider-specific.
    const incompatibleWith = { anthropic_web_search: "openrouter" };
    const isPickable = (k) => {
      const blocked = incompatibleWith[k];
      return !(blocked && providerEl && providerEl.value === blocked);
    };
    const updateWarning = (cur) => {
      if (!warnEl) return;
      const conflict = cur.some((k) => !isPickable(k));
      warnEl.classList.toggle("is-hidden", !conflict);
    };

    const get = () => hidden.value.split(",").map(s => s.trim()).filter(Boolean);
    const set = (arr) => {
      hidden.value = arr.join(",");
      // Programmatic value changes don't fire `input` natively — dispatch
      // so dirty-tracking and any provider-aware listeners see the update.
      hidden.dispatchEvent(new Event("input", { bubbles: true }));
      render();
    };

    // Single popover instance shared across all chips in this input.
    const popover = document.createElement("div");
    popover.className = "chip-popover is-hidden";
    root.appendChild(popover);

    const showPopover = (anchor, text) => {
      if (!text) text = "(no description available)";
      popover.textContent = text;
      const ar = anchor.getBoundingClientRect();
      const rr = root.getBoundingClientRect();
      popover.style.top = (ar.bottom - rr.top + 4) + "px";
      popover.style.left = (ar.left - rr.left) + "px";
      popover.classList.remove("is-hidden");
      popover.dataset.anchor = anchor.dataset.tool || "";
    };
    const hidePopover = () => {
      popover.classList.add("is-hidden");
      popover.dataset.anchor = "";
    };

    const render = () => {
      const cur = get();
      updateWarning(cur);
      list.innerHTML = "";
      hidePopover();
      cur.forEach((k) => {
        const chip = document.createElement("span");
        chip.className = "chip";
        chip.title = tools[k] || "";

        const info = document.createElement("button");
        info.type = "button";
        info.className = "chip-info";
        info.dataset.tool = k;
        info.setAttribute("aria-label", "Description of " + k);
        info.textContent = "ⓘ";
        info.addEventListener("click", (e) => {
          e.stopPropagation();
          if (popover.dataset.anchor === k && !popover.classList.contains("is-hidden")) {
            hidePopover();
          } else {
            showPopover(info, tools[k] || "");
          }
        });
        chip.appendChild(info);

        const label = document.createElement("span");
        label.className = "chip-label";
        label.textContent = k;
        chip.appendChild(label);

        const x = document.createElement("button");
        x.type = "button";
        x.className = "chip-x";
        x.setAttribute("aria-label", "Remove " + k);
        x.textContent = "×";
        x.addEventListener("click", () => set(cur.filter(t => t !== k)));
        chip.appendChild(x);
        list.appendChild(chip);
      });
      const remaining = allKeys.filter(k => !cur.includes(k) && isPickable(k));
      picker.innerHTML = "";
      remaining.forEach((k) => {
        const b = document.createElement("button");
        b.type = "button";
        b.title = tools[k] || "";
        b.textContent = k;
        b.addEventListener("click", () => {
          set([...get(), k]);
          picker.classList.add("is-hidden");
        });
        picker.appendChild(b);
      });
      addBtn.classList.toggle("is-hidden", remaining.length === 0);
    };

    addBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      picker.classList.toggle("is-hidden");
    });
    document.addEventListener("click", (e) => {
      if (!root.contains(e.target)) {
        picker.classList.add("is-hidden");
        hidePopover();
      } else if (!e.target.closest(".chip-info") && !e.target.closest(".chip-popover")) {
        hidePopover();
      }
    });
    providerEl?.addEventListener("change", render);
    render();
  });

  // ---- conditional sub-controls (data-show-when="<id-of-checkbox>") ----
  document.querySelectorAll("[data-show-when]").forEach((el) => {
    const cb = document.getElementById(el.dataset.showWhen);
    if (!cb) return;
    const upd = () => { el.classList.toggle("is-hidden", !cb.checked); };
    cb.addEventListener("change", upd);
    upd();
  });
})();
