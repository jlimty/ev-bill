// live.js keeps a status page current without reloading it (LIVE-3, VIEW-6).
// It opens the stream named by <body data-stream> and puts each "list",
// "locations" or "session" event's data (HTML rendered and escaped by
// html/template, from this origin) into the matching [data-live] element.
// It replaces the region's content, then puts focus back by key and keeps
// the viewport on the same item; items whose [data-watch] content changed
// flash, and [data-stream-status] says when the stream is down.
// <body data-stream-state> says "connected" once an event is applied, "done"
// once the server says the session is completed, and "closed" if the stream
// gives up.
(function () {
  'use strict';
  var body = document.body;
  if (!body.dataset.stream) return;
  var statusEl = document.querySelector('[data-stream-status]');
  if (!window.EventSource) {
    if (statusEl) statusEl.textContent = 'Updates paused';
    return;
  }
  var source = new EventSource('/events?' + body.dataset.stream);
  var focusable = 'a[href], button, input, select, textarea, [tabindex]';
  var stream = { closed: false, reconnecting: false, paused: false };
  var watchdog = 0;
  var lastHTML = null; // the fragment the region now shows
  var last = null; // its watched content, by item key
  var quiet = true; // the first event after (re)connecting never flashes

  // The status element shows the highest-precedence state, and is written
  // only when that changes, so screen readers hear changes of state only.
  function showStatus() {
    var text = stream.closed ? 'Updates paused · reloading soon'
      : stream.reconnecting ? 'Reconnecting…'
        : stream.paused ? 'Updates paused' : '';
    if (!statusEl || statusEl.textContent === text) return;
    statusEl.textContent = text;
    statusEl.classList.toggle('reconnecting', text === 'Reconnecting…');
  }
  // 75 s is 2.5 times the server's 30 s refresh. Heartbeat comments are not
  // events, so only a real update restarts it.
  function restartWatchdog() {
    clearTimeout(watchdog);
    watchdog = setTimeout(function () { stream.paused = true; showStatus(); }, 75000);
  }
  function eventArrived() {
    stream.reconnecting = false;
    stream.paused = false;
    showStatus();
  }

  // An item is an element with data-key. A [data-watch] element outside every
  // item is an item of its own, keyed by its position.
  function items(root) {
    var out = Object.create(null);
    root.querySelectorAll('[data-key]').forEach(function (el) { out[el.dataset.key] = el; });
    var loose = 0;
    root.querySelectorAll('[data-watch]').forEach(function (el) {
      if (!el.closest('[data-key]')) out['#' + loose++] = el;
    });
    return out;
  }
  // An item's watched content: the attributes and text of each [data-watch]
  // element in it, the item included.
  function watched(el) {
    var parts = [];
    var all = [el].concat(Array.prototype.slice.call(el.querySelectorAll('[data-watch]')));
    all.forEach(function (w) {
      if (w.hasAttribute('data-watch')) parts.push(w.cloneNode(false).outerHTML + w.textContent);
    });
    return parts.join('\n');
  }
  function focusables(el) {
    var list = Array.prototype.slice.call(el.querySelectorAll(focusable));
    return el.matches(focusable) ? [el].concat(list) : list;
  }
  function byKey(region, key) {
    return region.querySelector('[data-key="' + CSS.escape(key) + '"]');
  }
  // The focused element's place: its nearest item's key, and its index among
  // that item's focusables. The region stands in for a missing item, and then
  // the place holds only for an element with the same tag and href.
  function focusPlace(region) {
    var el = document.activeElement;
    if (!el || !region.contains(el)) return null;
    var item = el.closest('[data-key]');
    if (!item || !region.contains(item)) item = region;
    return {
      key: item === region ? null : item.dataset.key, index: focusables(item).indexOf(el),
      tag: el.tagName, href: el.getAttribute('href')
    };
  }
  function restoreFocus(region, place) {
    var item = place.key === null ? region : byKey(region, place.key);
    var target = item && place.index >= 0 ? focusables(item)[place.index] : null;
    if (target && place.key === null && (target.tagName !== place.tag || target.getAttribute('href') !== place.href)) {
      target = null;
    }
    if (!target) {
      target = document.querySelector('h1');
      if (!target) return;
      if (!target.hasAttribute('tabindex')) target.setAttribute('tabindex', '-1');
    }
    target.focus({ preventScroll: true });
  }

  function inView(el) {
    var r = el.getBoundingClientRect();
    return r.bottom > 0 && r.top < window.innerHeight;
  }
  // The item the viewport is kept on, with its distance from the top of the
  // viewport: the focused item while it is in view, or else, once the region
  // starts above the viewport, the first item in view. With neither, the
  // scroll offset is kept, so a page at its top stays there.
  function viewAnchor(region, place) {
    var el = place && place.key !== null ? byKey(region, place.key) : null;
    if (el && !inView(el)) el = null;
    if (!el && region.getBoundingClientRect().top < 0) {
      var keyed = region.querySelectorAll('[data-key]');
      for (var i = 0; !el && i < keyed.length; i++) {
        if (inView(keyed[i])) el = keyed[i];
      }
    }
    return el ? { key: el.dataset.key, top: el.getBoundingClientRect().top } : null;
  }

  function apply(e) {
    var region = document.querySelector('[data-live="' + e.type + '"]');
    // An identical fragment changes nothing, so the page is left alone (R19).
    if (region && e.data !== lastHTML) {
      var next = document.createElement('template');
      next.innerHTML = e.data;
      var now = Object.create(null);
      var nextItems = items(next.content);
      Object.keys(nextItems).forEach(function (key) { now[key] = watched(nextItems[key]); });
      var place = focusPlace(region);
      var anchor = viewAnchor(region, place);
      var x = window.scrollX, y = window.scrollY;
      region.replaceChildren(next.content);
      if (last && !quiet) {
        Object.keys(now).forEach(function (key) {
          if (key in last && last[key] !== now[key]) flash(nextItems[key]);
        });
      }
      last = now;
      lastHTML = e.data;
      if (place) restoreFocus(region, place);
      // Scroll only when needed, so a routine update never cuts short a
      // scroll the reader is making.
      var moved = anchor && byKey(region, anchor.key);
      var top = moved ? window.scrollY + moved.getBoundingClientRect().top - anchor.top : y;
      if (window.scrollX !== x || Math.abs(window.scrollY - top) >= 1) window.scrollTo(x, top);
    }
    quiet = false;
    body.dataset.streamState = 'connected';
    eventArrived();
    restartWatchdog();
  }
  function flash(el) {
    el.classList.add('flash');
    setTimeout(function () { el.classList.remove('flash'); }, 1000);
  }

  restartWatchdog();
  source.addEventListener('open', function () { quiet = true; });
  source.addEventListener('list', apply);
  source.addEventListener('locations', apply);
  source.addEventListener('session', apply);
  // A completed session will not change again: the server sends "done" after
  // its last fragment and ends the stream, so stop here, with no reload.
  source.addEventListener('done', function () {
    source.close();
    clearTimeout(watchdog);
    body.dataset.streamState = 'done';
    eventArrived();
  });
  // A dropped connection leaves the source CONNECTING and the browser retries
  // it. A non-200 answer (503 at the stream cap, 404 for a deleted session)
  // closes it for good, so reload the page once, 30 to 45 s later. The random
  // part spreads out viewers that were all turned away at the same moment.
  source.addEventListener('error', function () {
    var state = body.dataset.streamState;
    if (state === 'closed' || state === 'done') return;
    if (source.readyState !== EventSource.CLOSED) {
      stream.reconnecting = true;
      showStatus();
      return;
    }
    body.dataset.streamState = 'closed';
    clearTimeout(watchdog);
    stream.closed = true;
    showStatus();
    setTimeout(function () { location.reload(); }, 30000 + Math.floor(Math.random() * 15000));
  });
}());
