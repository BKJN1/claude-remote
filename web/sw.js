// Service worker: shows push notifications and opens the right session when
// one is tapped. It caches nothing and intercepts no requests.
'use strict';

self.addEventListener('install', function () { self.skipWaiting(); });
self.addEventListener('activate', function (ev) { ev.waitUntil(self.clients.claim()); });

self.addEventListener('push', function (ev) {
  var data = {};
  try { data = ev.data.json(); } catch (e) { /* ignore malformed payloads */ }
  ev.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function (cs) {
      // The user is already looking at the app; no need to interrupt them.
      for (var i = 0; i < cs.length; i++) {
        if (cs[i].visibilityState === 'visible') return;
      }
      return self.registration.showNotification(data.title || 'claude-remote', {
        body: data.body || '',
        tag: data.tag || undefined,
        renotify: true,
        icon: '/icon-192.png',
        data: { session: data.session || '' }
      });
    })
  );
});

self.addEventListener('notificationclick', function (ev) {
  ev.notification.close();
  var session = (ev.notification.data && ev.notification.data.session) || '';
  ev.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function (cs) {
      if (cs.length > 0) {
        cs[0].postMessage({ type: 'open-session', session: session });
        return cs[0].focus();
      }
      return self.clients.openWindow('/#s=' + encodeURIComponent(session));
    })
  );
});
