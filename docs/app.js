/* PodsMedic — landing page behaviour.
 *
 * The hero animation is not decoration and not a video: it is the product's
 * actual signal, reimplemented in miniature. Workloads orbit; a failing one
 * stops while its neighbours keep going. Letting a visitor trigger that with a
 * button explains the idea faster than any paragraph on the page, and it is
 * honest — this is what the built-in live view really does.
 *
 * No dependencies, matching the project's own stance. */
(() => {
  "use strict";

  const REDUCED = window.matchMedia &&
                  window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  /* ── orbit animation ──────────────────────────────────────────────────── */

  const COLOUR = { ok: "#4d6480", bad: "#ff4d5e", globe: "#7ec4ff" };

  // The event classes the real live view draws wires for, with its colours.
  // Reusing them means the page is showing the product's own vocabulary rather
  // than inventing a prettier one.
  const EVENTS = [
    { cls: "diagnose", colour: "#4aa8ff", out: true },  // globe → workload
    { cls: "heal",     colour: "#38d996", out: true },
    { cls: "verify",   colour: "#7ee0b8", out: true },
    { cls: "restart",  colour: "#ffab3d", out: false }, // workload → globe
  ];

  // Fibonacci sphere: evenly spaced points with no clustering at the poles.
  function sphere(n) {
    const pts = [], step = Math.PI * (3 - Math.sqrt(5));
    for (let i = 0; i < n; i++) {
      const y = 1 - (i / (n - 1)) * 2;
      const r = Math.sqrt(Math.max(0, 1 - y * y));
      const t = step * i;
      pts.push([Math.cos(t) * r, y, Math.sin(t) * r]);
    }
    return pts;
  }

  function createOrbit(canvas, opts) {
    const ctx = canvas.getContext("2d");
    const dots = sphere(opts.dots);
    let W = 0, H = 0, spin = 0, last = 0, clock = 0;
    let wires = [], nextWire = 1200;

    // Each shell is one namespace: its own radius, inclination and speed, with
    // alternating direction so crossings keep changing.
    const shells = opts.shells.map((s, i) => ({
      flatten: s.flatten, tilt: s.tilt,
      speed: (i % 2 ? -1 : 1) * s.speed,
      scale: s.scale,
    }));

    const workloads = opts.workloads.map((w, i) => ({
      shell: shells[w.shell],
      angle: w.angle,
      radius: w.radius || 4,
      stalled: false,
      label: w.label || "workload-" + (i + 1),
    }));

    function resize() {
      const dpr = Math.min(window.devicePixelRatio || 1, 2);
      const rect = canvas.getBoundingClientRect();
      W = rect.width; H = rect.height;
      canvas.width = Math.round(W * dpr);
      canvas.height = Math.round(H * dpr);
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    }

    const R = () => Math.min(W, H) * 0.26;

    function point(shell, theta) {
      const rx = R() * shell.scale, ry = rx * shell.flatten;
      const ux = Math.cos(theta) * rx, uy = Math.sin(theta) * ry;
      const c = Math.cos(shell.tilt), s = Math.sin(shell.tilt);
      return [W / 2 + ux * c - uy * s, H / 2 + ux * s + uy * c];
    }

    function drawGlobe() {
      const cx = W / 2, cy = H / 2, r = R();

      const glow = ctx.createRadialGradient(cx, cy, r * 0.1, cx, cy, r * 1.9);
      glow.addColorStop(0, "rgba(74,168,255,0.14)");
      glow.addColorStop(1, "rgba(74,168,255,0)");
      ctx.fillStyle = glow;
      ctx.beginPath(); ctx.arc(cx, cy, r * 1.9, 0, Math.PI * 2); ctx.fill();

      ctx.strokeStyle = "rgba(74,168,255,0.18)";
      ctx.lineWidth = 1;
      ctx.beginPath(); ctx.arc(cx, cy, r, 0, Math.PI * 2); ctx.stroke();

      const cosS = Math.cos(spin), sinS = Math.sin(spin);
      const tilt = 0.42, cosT = Math.cos(tilt), sinT = Math.sin(tilt);
      const CAM = 3.2;
      for (const [x0, y0, z0] of dots) {
        const x1 = x0 * cosS + z0 * sinS;
        const z1 = -x0 * sinS + z0 * cosS;
        const y2 = y0 * cosT - z1 * sinT;
        const z2 = y0 * sinT + z1 * cosT;
        const k = CAM / (CAM - z2);
        const depth = (z2 + 1) / 2;
        const alpha = z2 < 0 ? 0.08 + depth * 0.18 : 0.30 + depth * 0.55;
        ctx.fillStyle = `rgba(126,196,255,${alpha.toFixed(3)})`;
        ctx.beginPath();
        ctx.arc(cx + x1 * r * k, cy + y2 * r * k, (z2 < 0 ? 0.6 : 0.9 + depth) * k * 0.8, 0, Math.PI * 2);
        ctx.fill();
      }
    }

    // The heartbeat: a ring that fills once per notional sweep, then pulses.
    // In the product it exists because a healthy cluster is otherwise a
    // completely static picture, and "everything is fine" then looks exactly
    // like "the feed died".
    function drawHeartbeat() {
      const cx = W / 2, cy = H / 2, ring = R() * 1.14;
      const period = 9000;
      const p = (clock % period) / period;

      ctx.strokeStyle = "rgba(74,168,255,0.10)";
      ctx.lineWidth = 2;
      ctx.beginPath(); ctx.arc(cx, cy, ring, 0, Math.PI * 2); ctx.stroke();

      ctx.strokeStyle = "rgba(126,196,255,0.40)";
      ctx.lineWidth = 2;
      ctx.beginPath();
      ctx.arc(cx, cy, ring, -Math.PI / 2, -Math.PI / 2 + Math.PI * 2 * p);
      ctx.stroke();

      // A single expanding ring as the sweep lands.
      const since = clock % period;
      if (since < 900) {
        const k = since / 900;
        ctx.strokeStyle = `rgba(126,196,255,${(0.3 * (1 - k)).toFixed(3)})`;
        ctx.lineWidth = 1.5;
        ctx.beginPath(); ctx.arc(cx, cy, ring + k * R() * 0.8, 0, Math.PI * 2); ctx.stroke();
      }
    }

    // spawnWire fires one event between the globe and a workload. Direction is
    // the whole grammar in the real view: outward when podsmedic acts on a
    // workload, inward when the cluster reports something.
    function spawnWire(index, event) {
      const w = workloads[index];
      if (!w) return;
      wires.push({ w, colour: event.colour, out: event.out, born: clock,
                   life: event.out ? 2600 : 2200 });
      if (wires.length > 14) wires = wires.slice(-14);
    }

    function drawWires() {
      const cx = W / 2, cy = H / 2, r = R();
      wires = wires.filter((wire) => clock - wire.born < wire.life);

      ctx.save();
      ctx.globalCompositeOperation = "lighter";
      ctx.lineCap = "round";

      for (const wire of wires) {
        const [x, y] = point(wire.w.shell, wire.w.angle);
        const p = (clock - wire.born) / wire.life;
        const fade = p < 0.12 ? p / 0.12 : 1 - (p - 0.12) / 0.88;

        // Start at the globe's limb, not its centre: a line drawn through the
        // dot field loses its inner half in the noise.
        const dx = x - cx, dy = y - cy;
        const len = Math.hypot(dx, dy) || 1;
        const sx = cx + (dx / len) * r * 1.04, sy = cy + (dy / len) * r * 1.04;

        // Bow it away from centre so overlapping wires stay legible.
        const mx = (sx + x) / 2, my = (sy + y) / 2;
        const bow = Math.min(W, H) * 0.05;
        const bx = mx + (-dy / len) * bow, by = my + (dx / len) * bow;

        const at = (t) => {
          const u = (1 - t) * (1 - t), v = 2 * (1 - t) * t, s2 = t * t;
          return [u * sx + v * bx + s2 * x, u * sy + v * by + s2 * y];
        };

        ctx.strokeStyle = wire.colour;
        ctx.globalAlpha = 0.08 + fade * 0.18;
        ctx.lineWidth = 4;
        ctx.beginPath(); ctx.moveTo(sx, sy); ctx.quadraticCurveTo(bx, by, x, y); ctx.stroke();

        ctx.globalAlpha = 0.18 + fade * 0.6;
        ctx.lineWidth = 1.5;
        ctx.beginPath(); ctx.moveTo(sx, sy); ctx.quadraticCurveTo(bx, by, x, y); ctx.stroke();

        // A comet, not a dot: the tail is what makes the direction readable.
        const q = wire.out ? p : 1 - p;
        for (let i = 0; i < 5; i++) {
          const back = i * 0.05 * (wire.out ? 1 : -1);
          const t = Math.min(Math.max(q - back, 0), 1);
          const [px, py] = at(t);
          ctx.globalAlpha = fade * (1 - i / 5) * 0.85;
          ctx.fillStyle = wire.colour;
          ctx.beginPath(); ctx.arc(px, py, 3 - i * 0.4, 0, Math.PI * 2); ctx.fill();
        }

        if (p > 0.82) {
          const [ex, ey] = wire.out ? [x, y] : [sx, sy];
          const k = (p - 0.82) / 0.18;
          ctx.globalAlpha = (1 - k) * 0.5;
          ctx.strokeStyle = wire.colour;
          ctx.lineWidth = 1.5;
          ctx.beginPath(); ctx.arc(ex, ey, 4 + k * 12, 0, Math.PI * 2); ctx.stroke();
        }
      }
      ctx.restore();
      ctx.globalAlpha = 1;
    }

    function drawPaths() {
      const cx = W / 2, cy = H / 2;
      for (const s of shells) {
        const rx = R() * s.scale;
        ctx.strokeStyle = "rgba(111,143,181,0.20)";
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.ellipse(cx, cy, rx, rx * s.flatten, s.tilt, 0, Math.PI * 2);
        ctx.stroke();
      }
    }

    // front === true draws the half of each orbit nearer the viewer, so the
    // globe occludes what is behind it and the ellipse reads as a real orbit.
    function drawWorkloads(front) {
      for (const w of workloads) {
        const z = Math.sin(w.angle);
        if ((z >= 0) !== front) continue;

        const [x, y] = point(w.shell, w.angle);
        const near = (z + 1) / 2;
        const rad = w.radius * (0.75 + near * 0.35);
        const dim = front ? 1 : 0.45 + near * 0.25;
        const colour = w.stalled ? COLOUR.bad : COLOUR.ok;

        // The wake. A stopped workload has none, and its absence is the point:
        // every neighbour is a comet and this one is a full stop.
        if (!w.stalled && !REDUCED) {
          ctx.save();
          ctx.globalCompositeOperation = "lighter";
          for (let i = 1; i <= 8; i++) {
            const t = w.angle - i * 0.055 * Math.sign(w.shell.speed);
            const [tx, ty] = point(w.shell, t);
            const k = 1 - i / 9;
            ctx.globalAlpha = k * k * 0.5 * dim;
            ctx.fillStyle = colour;
            ctx.beginPath(); ctx.arc(tx, ty, rad * k * 0.7, 0, Math.PI * 2); ctx.fill();
          }
          ctx.restore();
          ctx.globalAlpha = 1;
        }

        if (front) {
          ctx.fillStyle = "rgba(7,10,15,0.72)";
          ctx.beginPath(); ctx.arc(x, y, rad + 2.4, 0, Math.PI * 2); ctx.fill();
        }

        if (w.stalled) {
          // Static rings, never a pulse: something throbbing reads as alive.
          ctx.strokeStyle = "rgba(255,77,94,0.55)";
          ctx.lineWidth = 1.2;
          ctx.beginPath(); ctx.arc(x, y, rad + 5, 0, Math.PI * 2); ctx.stroke();
          ctx.strokeStyle = "rgba(255,77,94,0.22)";
          ctx.beginPath(); ctx.arc(x, y, rad + 9.5, 0, Math.PI * 2); ctx.stroke();
        }

        ctx.globalAlpha = dim;
        ctx.fillStyle = colour;
        ctx.beginPath(); ctx.arc(x, y, rad, 0, Math.PI * 2); ctx.fill();
        ctx.globalAlpha = 1;
      }
    }

    function frame(ts) {
      const dt = last ? Math.min(ts - last, 120) : 16;
      last = ts;
      if (!REDUCED) {
        clock += dt;
        // Roughly one rotation every 22 seconds. The first attempt used a rate
        // taken straight from the product, where the globe is background to a
        // dashboard someone leaves open — here it read as a still image, which
        // is the opposite of what a landing page needs.
        spin += dt * 0.00028;
        for (const w of workloads) {
          if (!w.stalled) w.angle = (w.angle + w.shell.speed * dt / 1000) % (Math.PI * 2);
        }
        if (opts.events !== false) {
          nextWire -= dt;
          if (nextWire <= 0) {
            const healthy = workloads.map((w, i) => [w, i]).filter(([w]) => !w.stalled);
            if (healthy.length) {
              const pick = healthy[Math.floor(Math.random() * healthy.length)][1];
              spawnWire(pick, EVENTS[Math.floor(Math.random() * EVENTS.length)]);
            }
            nextWire = 1600 + Math.random() * 2200;
          }
        }
      }
      ctx.clearRect(0, 0, W, H);
      drawPaths();
      drawWorkloads(false);
      drawGlobe();
      drawHeartbeat();
      drawWires();
      drawWorkloads(true);
      requestAnimationFrame(frame);
    }

    resize();
    window.addEventListener("resize", resize);
    requestAnimationFrame(frame);

    return {
      workloads,
      stall(i, on) {
        const w = workloads[i];
        if (!w) return;
        w.stalled = on;
        // Cause before effect: a failure reports itself to the globe, and a
        // recovery is podsmedic confirming the workload came back. Same
        // grammar as the product — inbound is the cluster talking, outbound is
        // podsmedic acting.
        if (!REDUCED) {
          spawnWire(i, on ? { colour: COLOUR.bad, out: false }
                          : { colour: "#38d996", out: true });
        }
      },
      stalledCount() { return workloads.filter((w) => w.stalled).length; },
    };
  }

  /* ── hero orbit ───────────────────────────────────────────────────────── */

  const heroCanvas = document.getElementById("orbit");
  let hero = null;
  if (heroCanvas) {
    hero = createOrbit(heroCanvas, {
      dots: 260,
      shells: [
        { scale: 1.30, flatten: 0.30, tilt: 0.35, speed: 0.26 },
        { scale: 1.62, flatten: 0.52, tilt: 1.30, speed: 0.20 },
        { scale: 1.94, flatten: 0.36, tilt: 2.30, speed: 0.16 },
      ],
      workloads: [
        { shell: 0, angle: 0.0, radius: 5 },
        { shell: 0, angle: 2.1, radius: 4 },
        { shell: 1, angle: 1.0, radius: 5 },
        { shell: 1, angle: 3.6, radius: 4 },
        { shell: 2, angle: 0.6, radius: 5 },
        { shell: 2, angle: 4.0, radius: 4 },
      ],
    });
  }

  const crashBtn = document.getElementById("crash-btn");
  const demoStatus = document.getElementById("demo-status");

  function reportDemo() {
    if (!demoStatus || !hero) return;
    const stopped = hero.stalledCount();
    const moving = hero.workloads.length - stopped;
    demoStatus.textContent = stopped
      ? `${stopped} stopped · ${moving} still orbiting`
      : `${moving} workloads orbiting`;
  }

  if (crashBtn && hero) {
    let next = 0;
    crashBtn.addEventListener("click", () => {
      if (hero.stalledCount() >= 2) {
        // Recover everything, so the control is a demonstration rather than a
        // one-way trip into a broken-looking page.
        hero.workloads.forEach((_, i) => hero.stall(i, false));
        crashBtn.textContent = "Simulate a failure";
      } else {
        hero.stall(next % hero.workloads.length, true);
        next += 3;
        crashBtn.textContent = hero.stalledCount() >= 2 ? "Recover them" : "Simulate another";
      }
      reportDemo();
    });
    reportDemo();
  }

  /* ── the small mock inside the live-view section ──────────────────────── */

  const shotCanvas = document.getElementById("orbit-shot");
  if (shotCanvas) {
    const shot = createOrbit(shotCanvas, {
      dots: 170,
      shells: [
        { scale: 1.34, flatten: 0.34, tilt: 0.5, speed: 0.24 },
        { scale: 1.70, flatten: 0.55, tilt: 1.7, speed: 0.18 },
      ],
      workloads: [
        { shell: 0, angle: 0.3, radius: 4 },
        { shell: 0, angle: 2.4, radius: 4 },
        { shell: 0, angle: 4.5, radius: 3.5 },
        { shell: 1, angle: 1.2, radius: 4 },
        { shell: 1, angle: 3.9, radius: 4 },
      ],
    });
    // One permanently stopped, matching the "problems 1" in the mock header.
    shot.stall(2, true);
  }

  /* ── copy buttons ─────────────────────────────────────────────────────── */

  for (const btn of document.querySelectorAll(".copy")) {
    btn.addEventListener("click", async () => {
      const code = btn.parentElement.querySelector("code");
      if (!code) return;
      try {
        await navigator.clipboard.writeText(code.textContent.trim());
        btn.textContent = "Copied";
        btn.classList.add("done");
      } catch (e) {
        // Clipboard access can be refused (insecure origin, permissions). Say
        // so rather than silently doing nothing.
        btn.textContent = "Select it";
        const range = document.createRange();
        range.selectNodeContents(code);
        const sel = window.getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
      }
      setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("done"); }, 1800);
    });
  }

  /* ── mobile nav ───────────────────────────────────────────────────────── */

  const navToggle = document.getElementById("nav-toggle");
  const navMobile = document.getElementById("site-nav-mobile");
  if (navToggle && navMobile) {
    navToggle.addEventListener("click", () => {
      const open = navMobile.hasAttribute("hidden");
      navMobile.toggleAttribute("hidden", !open);
      navToggle.setAttribute("aria-expanded", String(open));
    });
    navMobile.addEventListener("click", (e) => {
      if (e.target.tagName === "A") {
        navMobile.setAttribute("hidden", "");
        navToggle.setAttribute("aria-expanded", "false");
      }
    });
  }

  /* ── reveal on scroll ─────────────────────────────────────────────────── */

  const revealables = document.querySelectorAll(".reveal");
  if (!("IntersectionObserver" in window) || REDUCED) {
    // No observer, or the viewer asked for less motion: show everything at
    // once. Content must never depend on an effect to become readable.
    revealables.forEach((el) => el.classList.add("shown"));
  } else {
    const io = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          entry.target.classList.add("shown");
          io.unobserve(entry.target);
        }
      }
    }, { rootMargin: "0px 0px -8% 0px", threshold: 0.05 });
    revealables.forEach((el) => io.observe(el));
  }
})();
