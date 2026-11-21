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

// What runs where: agents edit files in worktrees, and the same rules hydra
// applies decide which cells light up. Nothing below is a recording.
(function () {
  var el = document.getElementById("sim");
  if (!el) return;

  var STRIDE = 200, WATCH = 1000, HOLD = 5200;
  var reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var ORDER = ["web", "api", "worker", "db"];
  var SVC = {
    web: { port: 3000, paths: ["apps/web/", "packages/shared/"], up: "api", routed: true },
    api: { port: 8080, paths: ["services/api/", "packages/shared/"] },
    worker: { paths: ["services/worker/", "packages/shared/"], up: "api" },
    db: { port: 5432, shared: true }
  };

  var SCENES = [
    {
      label: "three agents",
      cap: "Three agents in three worktrees. Each starts only the service it touched, and the one fixing a typo starts nothing. A worktree that left the frontend alone uses main's, on a port of its own.",
      lanes: [{ name: "fix-cart-total", tag: "slot 1", slot: 1 }, { name: "api-pagination", tag: "slot 2", slot: 2 }, { name: "docs-typo", tag: "slot 3", slot: 3 }],
      script: [
        [500, 0, "say", "reading apps/web/cart.tsx"],
        [400, 1, "say", "reading services/api/list.go"],
        [400, 2, "say", "reading README.md"],
        [1100, 0, "edit", "apps/web/cart.tsx"],
        [500, 1, "edit", "services/api/list.go"],
        [600, 2, "edit", "README.md"],
        [100, 2, "note", "No service owns README.md, so nothing starts."],
        [900, 0, "say", "checking the total at localhost:3200"],
        [700, 1, "say", "checking page 2 of the orders list at localhost:3400"],
        [700, 2, "say", "done"]
      ]
    },
    {
      label: "change, then revert",
      cap: "The agent tries a fix in the worker, drops it, and fixes the frontend instead. The worker stops when its change is gone.",
      lanes: [{ name: "retry-backoff", tag: "slot 1", slot: 1 }],
      script: [
        [500, 0, "say", "reading services/worker/retry.go"],
        [1100, 0, "edit", "services/worker/retry.go"],
        [1100, 0, "say", "go test ./services/worker/..."],
        [1500, 0, "say", "wrong layer. git checkout -- services/worker"],
        [900, 0, "revert", "services/worker/"],
        [1000, 0, "edit", "apps/web/orders.tsx"],
        [900, 0, "say", "checking localhost:3200/orders"]
      ]
    },
    {
      label: "one branch that grows",
      cap: "A feature starts in the frontend and spreads. When the API starts here, the frontend is restarted to talk to it instead of main's.",
      lanes: [{ name: "order-notes", tag: "slot 1", slot: 1 }],
      script: [
        [500, 0, "say", "adding a notes field to the order page"],
        [1100, 0, "edit", "apps/web/notes.tsx"],
        [1300, 0, "say", "the API needs the field too"],
        [900, 0, "edit", "services/api/notes.go"],
        [100, 0, "note", "web restarted: its API_URL now points at :8280."],
        [1300, 0, "say", "moving the type into the shared package"],
        [900, 0, "edit", "packages/shared/types.ts"],
        [100, 0, "note", "packages/shared belongs to web, api and worker."]
      ]
    },
    {
      label: "stacked branches",
      cap: "Three branches stacked on each other, one agent in each. hydra runs once, in the top worktree, and starts each service from the worktree of the branch that changed it.",
      stack: true,
      lanes: [
        { name: "orders-api", tag: "on main", slot: 1, init: ["services/api/orders.go"] },
        { name: "orders-jobs", tag: "on orders-api", slot: 1 },
        { name: "orders-ui", tag: "on orders-jobs · hydra runs here, slot 1", slot: 1, init: ["apps/web/orders.tsx"] }
      ],
      script: [
        [600, 2, "say", "building the orders page"],
        [500, 0, "say", "tightening the orders query"],
        [700, 1, "say", "adding the nightly export job"],
        [1100, 1, "edit", "services/worker/export.go"],
        [1300, 0, "say", "editing services/api/orders.go"],
        [300, 0, "note", "Already running: the api's own reload picks this edit up."],
        [900, 2, "say", "checking localhost:3200/orders"]
      ]
    }
  ];

  var bar = el.querySelector(".sim-bar"), cap = el.querySelector(".sim-cap"), grid = el.querySelector(".grid");
  var count = el.querySelector(".sim-count"), log = el.querySelector(".sim-log"), prog = el.querySelector(".sim-prog i");
  var toggle = el.querySelector("[data-sim-toggle]");

  var scene, rows, changed, shown, steps, at, timer, typing = [], playing = false, auto = true, current = 0, started = false;

  SCENES.forEach(function (s, i) {
    var b = document.createElement("button");
    b.className = "sc"; b.textContent = s.label; b.setAttribute("aria-pressed", "false");
    b.addEventListener("click", function () { auto = false; started = true; load(i, reduce); });
    bar.insertBefore(b, bar.querySelector(".sim-ctl"));
  });
  var tabs = bar.querySelectorAll(".sc");

  function cellHTML(svc) {
    return '<div class="g-cell" data-svc="' + svc + '"><span class="off">·</span><span class="on"><i></i><b></b></span><span class="via"><i></i><b></b></span><span class="wire"></span></div>';
  }

  function build() {
    var html = '<div class="g-row head"><div>checkout</div>' + ORDER.map(function (s) { return "<div>" + s + "</div>"; }).join("") + "</div>";
    html += '<div class="g-row base"><div><div class="co-name">main<small>baseline</small></div><div class="co-line remark">Runs the whole stack, once.</div></div>' + ORDER.map(cellHTML).join("") + "</div>";
    scene.lanes.forEach(function (l) {
      html += '<div class="g-row"><div><div class="co-name">' + l.name + "<small>" + l.tag + "</small></div>" +
        '<div class="co-line agent idle">idle</div><div class="co-line changed"></div><div class="co-line remark"></div></div>' +
        ORDER.map(cellHTML).join("") + "</div>";
    });
    grid.innerHTML = html;
    var all = grid.querySelectorAll(".g-row");
    ORDER.forEach(function (svc) {
      var c = all[1].querySelector('[data-svc="' + svc + '"]');
      c.classList.add("is-on");
      c.querySelector("b").textContent = SVC[svc].port ? ":" + SVC[svc].port : "running";
    });
    rows = Array.prototype.slice.call(all, 2);
  }

  function match(svc, files) {
    return files.slice().sort().filter(function (f) {
      return SVC[svc].paths.some(function (p) { return f.indexOf(p) === 0; });
    })[0];
  }

  // The plan: which service runs in which row, on which port, wired to what.
  // via: a routed service the row didn't change. Main's copy answers on the
  // row's own port and calls the row's services.
  var via = {};
  function addVia(row, slot, local) {
    ORDER.forEach(function (svc) {
      var s = SVC[svc];
      if (!s.routed || svc in local) return;
      via[row + ":" + svc] = { row: row, svc: svc, text: ":" + (s.port + slot * STRIDE),
        wire: "main's copy\n" + s.up + " → " + (s.up in local ? "here" : "main") };
    });
  }

  function plan() {
    var out = {}, top = scene.lanes.length - 1;
    via = {};
    function add(row, svc, slot, why, local) {
      var s = SVC[svc];
      out[row + ":" + svc] = {
        row: row, svc: svc, why: why,
        text: s.port ? ":" + (s.port + slot * STRIDE) : "running",
        wire: s.up ? s.up + " → " + (s.up in local ? "here" : "main") : ""
      };
    }
    if (scene.stack) {
      var owner = {}, why = {};
      for (var i = top; i >= 0; i--) ORDER.forEach(function (svc) {
        if (SVC[svc].shared || svc in owner) return;
        var f = match(svc, changed[i]);
        if (f) { owner[svc] = i; why[svc] = "changed" + (i < top ? " in " + scene.lanes[i].name : "") + ": " + f; }
      });
      Object.keys(owner).forEach(function (svc) { add(owner[svc], svc, 1, why[svc], owner); });
      addVia(top, 1, owner);
    } else {
      scene.lanes.forEach(function (l, i) {
        var local = {}, w = {};
        ORDER.forEach(function (svc) {
          if (SVC[svc].shared) return;
          var f = match(svc, changed[i]);
          if (f) { local[svc] = true; w[svc] = "changed: " + f; }
        });
        Object.keys(local).forEach(function (svc) { add(i, svc, l.slot, w[svc], local); });
        addVia(i, l.slot, local);
      });
    }
    return out;
  }

  function stamp() {
    var d = new Date();
    return [d.getHours(), d.getMinutes(), d.getSeconds()].map(function (n) { return (n < 10 ? "0" : "") + n; }).join(":");
  }

  function say(cls, row, text) {
    var who = scene.stack ? scene.lanes[scene.lanes.length - 1].name : scene.lanes[row].name;
    var empty = log.querySelector(".empty");
    if (empty) empty.remove();
    var line = document.createElement("div");
    line.innerHTML = '<span class="who">' + esc(who) + " " + stamp() + " hydra:</span> <span class=\"" + cls + '">' + esc(text) + "</span>";
    log.appendChild(line);
    while (log.children.length > 5) log.removeChild(log.firstChild);
  }

  // Apply the plan the way the watcher does: start, stop, re-point.
  function apply(quiet) {
    var next = plan();
    Object.keys(shown).forEach(function (k) {
      if (next[k]) return;
      var c = rows[shown[k].row].querySelector('[data-svc="' + shown[k].svc + '"]');
      c.classList.remove("is-on", "pop", "rewire");
      say("minus", shown[k].row, "- " + shown[k].svc + " stopped; the baseline's copy is used again");
    });
    Object.keys(next).sort(function (a, b) { return next[a].svc < next[b].svc ? -1 : 1; }).forEach(function (k) {
      var n = next[k], c = rows[n.row].querySelector('[data-svc="' + n.svc + '"]');
      c.querySelector("b").textContent = n.text;
      c.querySelector(".wire").textContent = n.wire;
      if (!shown[k]) {
        c.classList.add("is-on");
        if (!quiet) { c.classList.remove("pop"); void c.offsetWidth; c.classList.add("pop"); }
        say("plus", n.row, "+ " + n.svc + " now runs here (" + n.why + ")");
      } else if (shown[k].wire !== n.wire && !quiet) {
        c.classList.remove("rewire"); void c.offsetWidth; c.classList.add("rewire");
      }
    });
    rows.forEach(function (r, i) {
      ORDER.forEach(function (svc) {
        if (!SVC[svc].routed) return;
        var c = r.querySelector('[data-svc="' + svc + '"]'), v = via[i + ":" + svc];
        c.classList.toggle("is-via", !!v);
        if (!v) return;
        var wire = c.querySelector(".wire"), was = wire.textContent;
        c.querySelector(".via b").textContent = v.text;
        wire.textContent = v.wire;
        if (was && was !== v.wire && !quiet) { c.classList.remove("rewire"); void c.offsetWidth; c.classList.add("rewire"); }
      });
    });
    shown = next;
    tally();
  }

  function tally() {
    var running = 4 + Object.keys(shown).length, full = 4 * (scene.lanes.length + 1), cells = "";
    for (var i = 0; i < full; i++) cells += i < running ? '<i class="f"></i>' : "<i></i>";
    count.innerHTML = "<span><b>" + running + "</b> processes running. A full stack in every checkout would be <b>" + full + "</b>.</span>" +
      '<span class="meter" aria-hidden="true">' + cells + "</span>";
  }

  function type(row, text, instant) {
    var line = rows[row].querySelector(".agent");
    clearInterval(typing[row]);
    line.classList.remove("idle");
    if (instant) { line.textContent = text; return; }
    var n = 0;
    line.innerHTML = '<span></span><i class="cur"></i>';
    var span = line.firstChild;
    typing[row] = setInterval(function () {
      n += 2;
      span.textContent = text.slice(0, n);
      if (n >= text.length) { clearInterval(typing[row]); line.textContent = text; }
    }, 28);
  }

  function showChanged(row) {
    rows[row].querySelector(".changed").innerHTML = changed[row].slice().sort().map(function (f) { return "<span>" + esc(f) + "</span>"; }).join("");
  }

  function flash(row) {
    rows[row].classList.remove("flash"); void rows[row].offsetWidth; rows[row].classList.add("flash");
  }

  function load(i, instant) {
    clearTimeout(timer);
    typing.forEach(clearInterval);
    current = i; scene = SCENES[i]; at = 0; shown = {}; steps = [];
    tabs.forEach(function (t, n) { t.setAttribute("aria-pressed", n === i ? "true" : "false"); });
    cap.textContent = scene.cap;
    log.innerHTML = '<div class="empty">hydra-watch has nothing to report yet.</div>';
    build();
    changed = scene.lanes.map(function (l) { return (l.init || []).slice(); });
    scene.lanes.forEach(function (l, n) { showChanged(n); });
    apply(true);

    scene.script.forEach(function (s) {
      var wait = s[0], row = s[1], kind = s[2], val = s[3];
      if (kind === "say") steps.push([wait, function (q) { type(row, val, q); }]);
      if (kind === "note") steps.push([wait, function () { rows[row].querySelector(".remark").textContent = val; }]);
      if (kind === "edit" || kind === "revert") {
        steps.push([wait, function (q) {
          if (kind === "edit") {
            if (changed[row].indexOf(val) < 0) changed[row].push(val);
            type(row, "editing " + val, q);
          } else {
            changed[row] = changed[row].filter(function (f) { return f.indexOf(val) !== 0; });
          }
          rows[row].querySelector(".remark").textContent = "";
          showChanged(row);
          if (!q) flash(row);
        }]);
        steps.push([WATCH, function (q) { apply(q); }]);
      }
    });

    if (instant) {
      steps.forEach(function (s) { s[1](true); });
      at = steps.length; playing = false;
      progress(); label();
    } else {
      playing = true; progress(); label(); tick();
    }
  }

  function progress() { prog.style.width = (steps.length ? (at / steps.length) * 100 : 0) + "%"; }
  function label() { toggle.textContent = at >= steps.length ? "replay" : playing ? "pause" : "play"; }

  function tick() {
    if (at >= steps.length) {
      playing = false; label();
      if (auto) timer = setTimeout(function () { load((current + 1) % SCENES.length); }, HOLD);
      return;
    }
    timer = setTimeout(function () { steps[at][1](false); at++; progress(); tick(); }, steps[at][0]);
  }

  toggle.addEventListener("click", function () {
    auto = false; started = true;
    if (at >= steps.length) return load(current, reduce);
    if (playing) { playing = false; clearTimeout(timer); } else { playing = true; tick(); }
    label();
  });

  // Scenes have different numbers of rows. Reserve the tallest, so the page
  // below doesn't move when the scene changes.
  function reserve() {
    var keep = scene, max = 0;
    grid.style.minHeight = "";
    SCENES.forEach(function (s) { scene = s; build(); max = Math.max(max, grid.offsetHeight); });
    scene = keep;
    grid.style.minHeight = max + "px";
  }

  // Only run while someone can see it.
  var inView = false;
  function resume() {
    clearTimeout(timer);
    if (!inView || document.hidden || reduce) return;
    if (!started) { started = true; load(0, false); }
    else if (playing || (auto && at >= steps.length)) tick();
  }
  document.addEventListener("visibilitychange", resume);

  reserve();
  load(0, true);
  if (reduce) toggle.hidden = true; // every scene is drawn finished; nothing to play
  var resized;
  window.addEventListener("resize", function () {
    clearTimeout(resized);
    resized = setTimeout(function () { var i = current; reserve(); load(i, true); if (!reduce) { at = steps.length; label(); } }, 200);
  });
  if (reduce || !("IntersectionObserver" in window)) return;
  new IntersectionObserver(function (entries) {
    inView = entries[0].isIntersecting;
    resume();
  }, { threshold: 0.3 }).observe(el);
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
        why[n] = hit ? "shared, always baseline (changed: " + hit + ")" : "shared";
        if (hit) warnings.push("warning: " + n + " is shared, but its paths changed: " + hit + "; the baseline's copy is used");
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
      return [n, local[n] ? "here" : routed ? "main, routed" : "baseline",
        s.port ? String(s.port + (local[n] || routed ? SLOT * STRIDE : 0)) : "-",
        routed ? "main's copy, tagged for this worktree" : why[n]];
    })));

    var wires = [];
    names.forEach(function (n) {
      if (!local[n]) {
        if (services[n].routed) Object.keys(services[n].upstreams).sort().forEach(function (env) {
          var t = services[n].upstreams[env];
          if (t === n || !routable(t)) return;
          wires.push([n, env, "-> " + t + " (" + (local[t] ? "local" : "baseline") + ")", value(t, !!local[t]),
            "per request: " + t + (local[t] ? " running here" : " not running here")]);
        });
        return;
      }
      Object.keys(services[n].upstreams || {}).sort().forEach(function (env) {
        var t = services[n].upstreams[env];
        wires.push([n, env, "-> " + t + " (" + (local[t] ? "local" : "baseline") + ")", value(t, !!local[t]),
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
