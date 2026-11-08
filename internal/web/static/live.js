// live.js keeps a status page current without reloading it (LIVE-3). It opens
// the stream named by <body data-stream> and puts each "list" or "session"
// event's data (HTML rendered and escaped by html/template, from this origin)
// into the matching [data-live] element. <body data-stream-state> says
// "connected" once an event is applied and "closed" if the stream gives up.
(function () {
  'use strict';
  var body = document.body;
  if (!body.dataset.stream || !window.EventSource) return;
  var source = new EventSource('/events?' + body.dataset.stream);
  function swap(e) {
    var target = document.querySelector('[data-live="' + e.type + '"]');
    if (target) target.innerHTML = e.data;
    body.dataset.streamState = 'connected';
  }
  source.addEventListener('list', swap);
  source.addEventListener('session', swap);
  // A dropped connection leaves the source CONNECTING and the browser retries
  // it. A non-200 answer (503 at the stream cap, 404 for a deleted session)
  // closes it for good, so reload the page once, 30 s later.
  source.addEventListener('error', function () {
    if (source.readyState !== EventSource.CLOSED || body.dataset.streamState === 'closed') return;
    body.dataset.streamState = 'closed';
    setTimeout(function () { location.reload(); }, 30000);
  });
}());
