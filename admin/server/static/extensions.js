/* Trusted business pages/settings share navigation cleanup and draft guards. */
'use strict';
var Papa = (function () {
    var pages = Object.create(null), settings = Object.create(null), active = [];
    var owners = new WeakMap(), revision = 0, forcing = 0;
    function hooks(value) { return typeof value === 'function' ? { destroy: value } : value || {}; }
    function destroy(value) {
        try { if (typeof value.destroy === 'function') value.destroy(); } catch (_) { /* Clean the remaining views too. */ }
    }
    function retire(mounts) {
        // Retire ownership before calling business code: cleanup may navigate.
        mounts.forEach(function (mount) {
            mount.alive = false;
            if (owners.get(mount.root) === mount) owners.delete(mount.root);
        });
        active = active.filter(function (mount) { return mounts.indexOf(mount) < 0; });
        mounts.forEach(function (mount) {
            var previous = mount.hooks; mount.hooks = [];
            previous.forEach(destroy);
            if (!owners.has(mount.root)) mount.root.replaceChildren();
        });
    }
    function begin(root) {
        if (!root || forcing) return null;
        revision++;
        var previous = active.filter(function (mount) { return mount.root === root; });
        var mount = { root: root, hooks: [], alive: true };
        owners.set(root, mount); active.push(mount);
        retire(previous);
        if (!mount.alive || owners.get(root) !== mount) return null;
        root.replaceChildren();
        return mount;
    }
    function keep(mount, value) {
        value = hooks(value);
        // The renderer itself may have navigated while returning its cleanup.
        if (mount.alive && owners.get(mount.root) === mount) mount.hooks.push(value);
        else destroy(value);
    }
    return {
        page: function (key, fn) { if (typeof fn === 'function') pages[key] = fn; },
        has: function (key) { return typeof pages[key] === 'function'; },
        render: function (key, el, meta) {
            if (!this.has(key)) return false;
            var mount = begin(el); if (!mount) return false;
            try { keep(mount, pages[key](el, meta || {})); }
            catch (error) { retire([mount]); throw error; }
            return true;
        },
        settings: function (key, label, fn) {
            if (/^[a-zA-Z0-9_-]+$/.test(key) && typeof fn === 'function') settings[key] = { label: label, render: fn };
        },
        renderSettings: function (root) {
            var mount = begin(root); if (!mount) return false;
            var keys = Object.keys(settings);
            try {
                for (var i = 0; i < keys.length && mount.alive; i++) {
                    var key = keys[i], section = document.createElement('section'), title = document.createElement('h2'), body = document.createElement('div');
                    title.textContent = settings[key].label; body.className = 'card';
                    section.dataset.settingsKey = key; section.append(title, body); root.append(section);
                    keep(mount, settings[key].render(body, { key: key }));
                }
            } catch (error) { retire([mount]); throw error; }
            return mount.alive;
        },
        leave: function (force) {
            var previous = active.slice(), seen = revision;
            if (!force) {
                for (var i = 0; i < previous.length; i++) {
                    for (var j = 0; j < previous[i].hooks.length; j++) {
                        var guard = previous[i].hooks[j].beforeLeave;
                        if (typeof guard === 'function') {
                            try { if (guard() === false) return false; } catch (_) { return false; }
                            if (revision !== seen) return false;
                        }
                    }
                }
            }
            revision++;
            var cleanupRevision = revision;
            // Logout is terminal for the old identity, including cleanup reentry.
            if (force) forcing++;
            try { retire(previous); } finally { if (force) forcing--; }
            return !!force || revision === cleanupRevision;
        }
    };
})();
