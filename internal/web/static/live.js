// live.js keeps a status page current without reloading it (LIVE-3). It opens
// the event stream named by <body data-stream>, and on each "list" or
// "session" event replaces the matching [data-live] element's content with
// the event's data: HTML that the server rendered and escaped with
// html/template, received from this same origin. It never builds HTML itself.
(function () {
  'use strict';
  var stream = document.body.dataset.stream;
  if (!stream || !window.EventSource) {
    return;
  }
  var source = new EventSource('/events?' + stream);
  function swap(e) {
    var target = document.querySelector('[data-live="' + e.type + '"]');
    if (target) {
      target.innerHTML = e.data;
    }
  }
  source.addEventListener('list', swap);
  source.addEventListener('session', swap);
  // On 'error' there is nothing to do: the browser reconnects by itself.
}());
