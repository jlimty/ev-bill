// live.js keeps a status page current without reloading it (LIVE-3). It opens
// the stream named by <body data-stream> and puts each "list", "locations" or
// "session" event's data (HTML rendered and escaped by html/template, from
// this origin) into the matching [data-live] element. <body data-stream-state> says
// "connected" once an event is applied, "done" once the server says the
// session is completed, and "closed" if the stream gives up.
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
  source.addEventListener('locations', swap);
  source.addEventListener('session', swap);
  // A completed session will not change again: the server sends "done" after
  // its last fragment and ends the stream, so stop here, with no reload.
  source.addEventListener('done', function () {
    source.close();
    body.dataset.streamState = 'done';
  });
  // A dropped connection leaves the source CONNECTING and the browser retries
  // it. A non-200 answer (503 at the stream cap, 404 for a deleted session)
  // closes it for good, so reload the page once, 30 to 45 s later. The random
  // part spreads out viewers that were all turned away at the same moment.
  source.addEventListener('error', function () {
    var state = body.dataset.streamState;
    if (source.readyState !== EventSource.CLOSED || state === 'closed' || state === 'done') return;
    body.dataset.streamState = 'closed';
    setTimeout(function () { location.reload(); }, 30000 + Math.floor(Math.random() * 15000));
  });
}());
