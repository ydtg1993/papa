/* Trusted business pages/settings share navigation cleanup and draft guards. */
'use strict';
var Papa = (function () {
    var pages = {}, settings = {}, active = [];
    function keep(el, hooks) {
        if (typeof hooks === 'function') hooks = { destroy: hooks };
        active.push({ el: el, hooks: hooks || {} });
    }
    return {
        page: function (key, fn) { if (typeof fn === 'function') pages[key] = fn; },
        has: function (key) { return typeof pages[key] === 'function'; },
        render: function (key, el, meta) {
            if (!this.has(key)) return false;
            keep(el, pages[key](el, meta || {})); return true;
        },
        settings: function (key, label, fn) {
            if (/^[a-zA-Z0-9_-]+$/.test(key) && typeof fn === 'function') settings[key] = { label: label, render: fn };
        },
        renderSettings: function (root) {
            if (!root) return;
            root.replaceChildren();
            Object.keys(settings).forEach(function (key) {
                var section = document.createElement('section'), title = document.createElement('h2'), body = document.createElement('div');
                title.textContent = settings[key].label; body.className = 'card';
                section.dataset.settingsKey = key; section.append(title, body); root.append(section);
                keep(body, settings[key].render(body, { key: key }));
            });
        },
        leave: function (force) {
            if (!force) {
                for (var i = 0; i < active.length; i++) {
                    var guard = active[i].hooks.beforeLeave;
                    if (typeof guard === 'function') {
                        try { if (guard() === false) return false; } catch (_) { return false; }
                    }
                }
            }
            var previous = active; active = [];
            previous.forEach(function (entry) {
                try { if (typeof entry.hooks.destroy === 'function') entry.hooks.destroy(); } catch (_) { /* Finish cleaning other views. */ }
                entry.el.replaceChildren();
            });
            return true;
        }
    };
})();
