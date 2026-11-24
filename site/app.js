// Theme: follow the system until the visitor picks one.
(function () {
  var root = document.documentElement;
  try {
    var saved = localStorage.getItem("theme");
    if (saved === "light" || saved === "dark") root.dataset.theme = saved;
  } catch (e) {}
  document.addEventListener("click", function (e) {
    if (!e.target.closest("[data-theme-toggle]")) return;
    var dark = root.dataset.theme
      ? root.dataset.theme === "dark"
      : window.matchMedia("(prefers-color-scheme: dark)").matches;
    root.dataset.theme = dark ? "light" : "dark";
    try { localStorage.setItem("theme", root.dataset.theme); } catch (err) {}
  });
})();

function esc(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// YAML: lines inside an x-hydra block are marked, so a stylesheet can dim
// the rest of the file and show them as the lines you add.
function yaml(src) {
  var inside = -1, any = false;
  var html = src.split("\n").map(function (line) {
    var indent = line.match(/^\s*/)[0].length, blank = !line.trim();
    if (/^\s*x-hydra:/.test(line)) inside = indent;
    else if (!blank && inside >= 0 && indent <= inside) inside = -1;
    var xh = inside >= 0 && !blank;
    any = any || xh;

    var comment = "", m = line.match(/^(.*?)(\s+#.*|#.*)$/);
    if (m && (m[1].match(/"/g) || []).length % 2 === 0) { line = m[1]; comment = m[2]; }
    var out = esc(line).replace(/^(\s*-?\s*)([A-Za-z_][\w.-]*)(:)/, '$1<span class="k">$2</span>$3');
    if (comment) out += '<span class="c">' + esc(comment) + "</span>";
    return '<span class="ln' + (xh ? " xh" : "") + '">' + (out || " ") + "</span>";
  }).join("");
  return { html: html, xh: any };
}

function shell(src) {
  return src.split("\n").map(function (line) {
    if (/^\s*#/.test(line)) return '<span class="c">' + esc(line) + "</span>";
    var m = line.match(/^(\$ )(.*?)(\s+#.*)?$/);
    if (m) return '<span class="p">$ </span>' + esc(m[2]) + (m[3] ? '<span class="c">' + esc(m[3]) + "</span>" : "");
    return '<span class="d">' + esc(line) + "</span>";
  }).join("\n");
}

document.querySelectorAll("pre[data-lang]").forEach(function (pre) {
  var src = pre.textContent.replace(/^\n/, "").replace(/\s+$/, "");
  pre.dataset.src = src;
  if (pre.dataset.lang === "yaml") {
    var y = yaml(src);
    pre.innerHTML = y.html;
    if (y.xh) pre.classList.add("has-xh");
  } else {
    pre.innerHTML = pre.dataset.lang === "sh" ? shell(src) : esc(src);
  }
});

// Copy: a command without its prompt, a block, or every block of a file.
document.addEventListener("click", function (e) {
  var btn = e.target.closest("[data-copy], .copy");
  if (!btn || !navigator.clipboard) return;
  var text = btn.dataset.copy;
  if (!text) {
    var scope = btn.closest(".anno, .code");
    text = Array.prototype.map.call(scope.querySelectorAll("pre"), function (pre) {
      var src = pre.dataset.src || pre.textContent;
      if (pre.dataset.lang !== "sh") return src;
      return src.split("\n").filter(function (l) { return !/^\s*#/.test(l); })
        .map(function (l) { return l.replace(/^\$ /, "").replace(/\s+#.*$/, ""); }).join("\n");
    }).join("\n\n");
  }
  navigator.clipboard.writeText(text).then(function () {
    var label = btn.querySelector(".act") || btn;
    var was = label.textContent;
    btn.classList.add("done");
    label.textContent = "copied";
    setTimeout(function () { btn.classList.remove("done"); label.textContent = was; }, 1300);
  });
});

// What runs where, in five steps. Each step is a whole state, so the reader
// can go back and forth; the only motion is the cell that just changed.
(function () {
  var el = document.getElementById("sim");
  if (!el) return;

  var ORDER = ["web", "api", "worker", "db"];
  var MAIN = { web: ":3000", api: ":8080", worker: "running", db: ":5432" };
  var ROWS = [
    { name: "fix-cart", slot: 1 },
    { name: "api-paging", slot: 2 }
  ];
  // cells: "row:svc" → [what the cell says, its second line, "here" | "via"]
  var STEPS = [
    {
      cap: "Main runs the whole stack, once. The two worktrees haven't changed anything yet, so nothing runs in them: whatever they need is main's.",
      changed: [[], []], cells: {}
    },
    {
      cap: "An agent in fix-cart edits apps/web/cart.tsx. That file is in web's paths, so hydra starts web in this worktree, on a port of its own. Its API calls go to main's api, which the branch didn't touch.",
      changed: [["apps/web/cart.tsx"], []],
      cells: { "0:web": [":3200", "api → main's", "here"] }, focus: "0:web"
    },
    {
      cap: "In api-paging, another agent edits services/api/list.go. Only the api starts there, on :8480, and it uses main's database.",
      changed: [["apps/web/cart.tsx"], ["services/api/list.go"]],
      cells: { "0:web": [":3200", "api → main's", "here"], "1:api": [":8480", "db → main's", "here"], "1:web": ["main's :3400", "api → here", "via"] },
      focus: "1:api"
    },
    {
      cap: "api-paging has no frontend of its own, and its agent still has a page to check: main's web answers on this worktree's port, :3400, and sends the page's API calls to this worktree's api.",
      changed: [["apps/web/cart.tsx"], ["services/api/list.go"]],
      cells: { "0:web": [":3200", "api → main's", "here"], "1:api": [":8480", "db → main's", "here"], "1:web": ["main's :3400", "api → here", "via"] },
      focus: "1:web"
    },
    {
      cap: "The first agent drops its change. Within two seconds web stops in fix-cart, and the worktree uses main's copy again.",
      changed: [[], ["services/api/list.go"]],
      cells: { "1:api": [":8480", "db → main's", "here"], "1:web": ["main's :3400", "api → here", "via"] },
      focus: "0:web"
    }
  ];

  var dots = el.querySelector(".sim-steps"), cap = el.querySelector(".sim-cap");
  var grid = el.querySelector(".grid"), count = el.querySelector(".sim-count");
  var toggle = el.querySelector("[data-sim-toggle]");
  var reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var at = 0, timer = null, playing = !reduce, inView = false;

  STEPS.forEach(function (s, i) {
    var b = document.createElement("button");
    b.textContent = i + 1;
    b.setAttribute("aria-label", "Step " + (i + 1));
    b.addEventListener("click", function () { stop(); show(i); });
    dots.appendChild(b);
  });

  function cell(svc, text, note, kind) {
    return '<div class="g-cell' + (kind ? " is-" + kind : "") + '" data-svc="' + svc + '"><span class="v">' + esc(text) + "</span>" +
      (note ? '<span class="n">' + esc(note) + "</span>" : "") + "</div>";
  }

  function show(i) {
    at = i;
    var st = STEPS[i];
    var html = '<div class="g-row head"><div>checkout</div>' + ORDER.map(function (s) { return "<div>" + s + "</div>"; }).join("") + "</div>";
    html += '<div class="g-row base"><div><div class="co-name">main<small>slot 0</small></div></div>' +
      ORDER.map(function (s) { return cell(s, MAIN[s], "", "main"); }).join("") + "</div>";
    ROWS.forEach(function (r, n) {
      var files = st.changed[n];
      html += '<div class="g-row"><div><div class="co-name">' + r.name + "<small>slot " + r.slot + "</small></div>" +
        '<div class="co-line">' + (files.length ? "changed <span>" + files.map(esc).join("</span> <span>") + "</span>" : "nothing changed") + "</div></div>" +
        ORDER.map(function (s) {
          var c = st.cells[n + ":" + s];
          return c ? cell(s, c[0], c[1], c[2]) : cell(s, "main's", "", "");
        }).join("") + "</div>";
    });
    grid.innerHTML = html;
    if (st.focus) {
      var f = st.focus.split(":");
      var c = grid.querySelectorAll(".g-row")[2 + Number(f[0])].querySelector('[data-svc="' + f[1] + '"]');
      c.classList.add("focus");
    }
    cap.textContent = st.cap;
    var running = 4 + Object.keys(st.cells).filter(function (k) { return st.cells[k][2] === "here"; }).length;
    count.innerHTML = "<b>" + running + "</b> processes running. A full stack in every checkout would be <b>" + 4 * (ROWS.length + 1) + "</b>.";
    dots.querySelectorAll("button").forEach(function (b, n) { b.setAttribute("aria-pressed", n === i ? "true" : "false"); });
    label();
  }

  function label() { toggle.textContent = playing ? "pause" : at === STEPS.length - 1 ? "replay" : "play"; }
  function stop() { playing = false; clearTimeout(timer); label(); }
  function schedule() {
    clearTimeout(timer);
    if (!playing || !inView || document.hidden) return;
    timer = setTimeout(function () {
      if (at === STEPS.length - 1) { playing = false; label(); return; }
      show(at + 1); schedule();
    }, at === 0 ? 3500 : 6000);
  }

  toggle.addEventListener("click", function () {
    if (playing) return stop();
    playing = true;
    if (at === STEPS.length - 1) show(0); else label();
    schedule();
  });
  document.addEventListener("visibilitychange", schedule);

  // Every step has the same rows; reserve the tallest caption so the page
  // below doesn't move.
  var tallest = 0;
  STEPS.forEach(function (s, i) { show(i); tallest = Math.max(tallest, cap.offsetHeight); });
  cap.style.minHeight = tallest + "px";
  show(0);
  if (reduce) { toggle.hidden = true; return; }
  if (!("IntersectionObserver" in window)) { inView = true; schedule(); return; }
  new IntersectionObserver(function (entries) {
    inView = entries[0].isIntersecting;
    schedule();
  }, { threshold: 0.4 }).observe(el);
})();

// The planner: hydra's rules on a four-service stack, slot 1.
(function () {
  var out = document.getElementById("plan-out");
  if (!out) return;

  var SLOT = 1, STRIDE = 200, HOST = "127.0.0.1";
  var services = {
    api: { port: 8080, paths: ["services/api/", "packages/shared/"], upstreams: {} },
    db: { shared: true, paths: ["db/migrations/"] },
    deps: { follow: true },
    web: { port: 3000, paths: ["apps/web/", "packages/shared/"], needs: ["deps"], routed: true,
           upstreams: { API_URL: "api", JOBS_QUEUE: "worker", NEXT_PUBLIC_SITE_URL: "web" } },
    worker: { paths: ["services/worker/", "packages/shared/"], url: "jobs-{slot}", baselineUrl: "jobs",
              upstreams: { API_URL: "api" } }
  };
  var names = Object.keys(services).sort();

  function value(name, local) {
    var s = services[name];
    if (!local && s.baselineUrl) return s.baselineUrl;
    var slot = local ? SLOT : 0;
    return (s.url || "http://{host}:{port}")
      .replace("{host}", HOST).replace("{port}", s.port + slot * STRIDE).replace("{slot}", slot);
  }

  function pad(rows) {
    var widths = [];
    rows.forEach(function (r) { r.forEach(function (c, i) { widths[i] = Math.max(widths[i] || 0, c.length); }); });
    return rows.map(function (r) {
      return r.map(function (c, i) { return i === r.length - 1 ? c : c + " ".repeat(widths[i] - c.length); }).join("  ");
    });
  }

  function render() {
    var changed = [];
    document.querySelectorAll(".fchip[aria-pressed='true']").forEach(function (b) { changed.push(b.dataset.path); });
    changed.sort();

    var local = {}, why = {}, warnings = [];
    names.forEach(function (n) {
      var s = services[n];
      var hit = (s.paths || []).length && changed.filter(function (f) {
        return s.paths.some(function (p) { return f.indexOf(p) === 0; }) && !/_test\.go$/.test(f);
      })[0];
      if (s.shared) {
        why[n] = hit ? "shared, always main's (changed: " + hit + ")" : "shared";
        if (hit) warnings.push("warning: " + n + " is shared, but its paths changed: " + hit + "; main's copy is used");
      } else if (hit) {
        local[n] = true; why[n] = "changed: " + hit;
      } else {
        why[n] = "unchanged";
      }
    });
    // A routed service stays on main's copy unless one of its calls can't be
    // routed per request: only values that carry a port can.
    function routable(t) { return (services[t].url || "{port}").indexOf("{port}") >= 0; }
    names.forEach(function (n) {
      var s = services[n];
      if (!s.routed || local[n]) return;
      var env = Object.keys(s.upstreams).sort().filter(function (e) {
        var t = s.upstreams[e];
        return t !== n && local[t] && !routable(t);
      })[0];
      if (env) { local[n] = true; why[n] = env + " points at " + s.upstreams[env] + " here and can't be routed per request"; }
    });
    names.forEach(function (n) {
      if (!services[n].follow) return;
      var by = names.filter(function (m) { return local[m] && (services[m].needs || []).indexOf(n) >= 0; })[0];
      if (by) { local[n] = true; why[n] = "needed by " + by; }
    });

    var rows = pad([["SERVICE", "RUNS", "PORT", "WHY"]].concat(names.map(function (n) {
      var s = services[n];
      var routed = s.routed && !local[n];
      return [n, local[n] ? "here" : routed ? "main, routed" : "main",
        s.port ? String(s.port + (local[n] || routed ? SLOT * STRIDE : 0)) : "-",
        routed ? "main's copy, tagged for this worktree" : why[n]];
    })));

    var wires = [];
    names.forEach(function (n) {
      if (!local[n]) {
        if (services[n].routed) Object.keys(services[n].upstreams).sort().forEach(function (env) {
          var t = services[n].upstreams[env];
          if (t === n || !routable(t)) return;
          wires.push([n, env, "-> " + t + " (" + (local[t] ? "here" : "main") + ")", value(t, !!local[t]),
            "per request: " + t + (local[t] ? " running here" : " not running here")]);
        });
        return;
      }
      Object.keys(services[n].upstreams || {}).sort().forEach(function (env) {
        var t = services[n].upstreams[env];
        wires.push([n, env, "-> " + t + " (" + (local[t] ? "here" : "main") + ")", value(t, !!local[t]),
          "default: " + t + (local[t] ? " running here" : " not running here")]);
      });
    });

    var html = '<span class="p">$ </span>hydra plan\n';
    html += '<span class="d">worktree, slot 1: ~/code/shop-checkout</span>\n\n';
    html += '<span class="d">' + esc(rows[0]) + "</span>\n";
    rows.slice(1).forEach(function (r, i) {
      var n = names[i];
      html += (local[n] ? '<span class="x">' + esc(r) + "</span>" : /shared, always/.test(r) ? '<span class="w">' + esc(r) + "</span>" : esc(r)) + "\n";
    });
    html += '\n<span class="d">wiring:</span>\n';
    if (wires.length) {
      pad(wires).forEach(function (w) { html += "  " + esc(w) + "\n"; });
    } else {
      var any = names.some(function (n) { return local[n]; });
      html += '<span class="d">  ' + (any ? "(what runs here points at no other service)" : "(nothing runs here)") + "</span>\n";
    }
    if (warnings.length) html += "\n" + warnings.map(function (w) { return '<span class="w">' + esc(w) + "</span>"; }).join("\n") + "\n";
    // Changed files that no service lists in its paths (a test file under
    // services/api is listed, and excluded on purpose).
    var unclaimed = changed.filter(function (f) {
      return !names.some(function (n) { return (services[n].paths || []).some(function (p) { return f.indexOf(p) === 0; }); });
    });
    if (unclaimed.length) html += '\n<span class="w">' + esc("note: changed, but in no service's paths, so nothing runs here for them: " + unclaimed.join(", ")) + "</span>\n";
    out.innerHTML = html;
  }

  document.querySelectorAll(".fchip").forEach(function (b) {
    b.addEventListener("click", function () {
      b.setAttribute("aria-pressed", b.getAttribute("aria-pressed") === "true" ? "false" : "true");
      render();
    });
  });
  render();
})();

// Docs sidebar: mark the section in view.
(function () {
  var links = document.querySelectorAll(".side a[href^='#']");
  if (!links.length || !("IntersectionObserver" in window)) return;
  var visible = new Set();
  var io = new IntersectionObserver(function (entries) {
    entries.forEach(function (en) { en.isIntersecting ? visible.add(en.target.id) : visible.delete(en.target.id); });
    var first = null;
    links.forEach(function (a) { if (!first && visible.has(a.getAttribute("href").slice(1))) first = a; });
    if (first) links.forEach(function (a) { a.classList.toggle("active", a === first); });
  }, { rootMargin: "-10px 0px -60% 0px" });
  links.forEach(function (a) {
    var el = document.getElementById(a.getAttribute("href").slice(1));
    if (el) io.observe(el);
  });
})();
