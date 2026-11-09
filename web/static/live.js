// Keeps pages current without reloading, and loads more library cards on
// scroll. Without this file everything still works as plain links and forms.
(() => {
  const pollEvery = 5000;
  const cards = () => document.querySelector("ul.cards");
  const liveArea = () => document.querySelector("[data-live]");
  const more = () => document.getElementById("more");
  let version = null;
  let refreshing = false;
  let refreshAgain = false;
  let posting = false;
  let loadingMore = false;
  let held = false;

  async function fetchPage(url) {
    const res = await fetch(url);
    if (res.redirected && new URL(res.url).pathname === "/login") {
      location.href = "/login";
      throw new Error("signed out");
    }
    if (!res.ok) throw new Error("HTTP " + res.status);
    return new DOMParser().parseFromString(await res.text(), "text/html");
  }

  function swap(selector, doc) {
    const now = document.querySelector(selector);
    const next = doc.querySelector(selector);
    if (now && next) {
      if (now.outerHTML !== next.outerHTML) now.replaceWith(next);
    } else if (now) {
      now.remove();
    }
  }

  function swapMore(doc) {
    const next = doc.getElementById("more");
    const now = more();
    if (now && next) now.href = next.href;
    else if (now) now.remove();
    else if (next) cards().after(next);
    watchMore();
  }

  function swapCards(doc) {
    const list = cards();
    const next = doc.querySelector("ul.cards");
    if (!list || !next) return;
    const same = list.children.length === next.children.length &&
      [...list.children].every((li, i) => li.id === next.children[i].id);
    if (!same) {
      list.replaceChildren(...next.children);
      return;
    }
    // Only what changed, so posters and scroll position stay put.
    [...next.children].forEach((li, i) => {
      if (list.children[i].outerHTML !== li.outerHTML) list.children[i].replaceWith(li);
    });
  }

  async function refresh() {
    if (refreshing) {
      refreshAgain = true;
      return;
    }
    refreshing = true;
    try {
      const openCard = document.querySelector("[popover]:popover-open")?.id;
      const url = new URL(location.href);
      url.hash = "";
      if (cards()) url.searchParams.set("limit", Math.max(cards().children.length, 1));
      const doc = await fetchPage(url);
      swap("header nav", doc);
      swap("#count", doc);
      swapCards(doc);
      if (cards()) swapMore(doc);
      const area = liveArea();
      const nextArea = doc.querySelector("[data-live]");
      // Ticks for a batch would be lost, so hold the update until they're cleared.
      if (area?.querySelector("input[type=checkbox][form]:checked")) {
        held = true;
      } else if (area && nextArea && area.innerHTML !== nextArea.innerHTML) {
        area.innerHTML = nextArea.innerHTML;
      }
      if (openCard) {
        const el = document.getElementById(openCard);
        if (el && !el.matches(":popover-open")) el.showPopover();
      }
    } finally {
      refreshing = false;
      if (refreshAgain) {
        refreshAgain = false;
        refresh().catch(() => {});
      }
    }
  }

  function showError(text) {
    document.querySelector(".flash-error")?.remove();
    const div = document.createElement("div");
    div.className = "flash-error";
    div.setAttribute("role", "alert");
    div.textContent = text;
    // Inside an open card, or it would show behind it.
    (document.querySelector("[popover]:popover-open > div") || document.querySelector("main")).prepend(div);
  }

  document.addEventListener("submit", async (e) => {
    const form = e.target;
    if (form.method !== "post" || !form.closest("ul.cards, [data-live]")) return;
    e.preventDefault();
    if (posting) return; // a double click must not send it twice
    posting = true;
    try {
      const res = await fetch(form.action, {
        method: "POST",
        body: new URLSearchParams(new FormData(form)),
        redirect: "manual",
      });
      if (res.type !== "opaqueredirect" && !res.ok) showError((await res.text()).trim());
      await refresh();
    } catch {
      showError("couldn't reach the server");
    } finally {
      posting = false;
    }
  });

  async function loadMore() {
    const link = more();
    if (!link || loadingMore) return;
    loadingMore = true;
    try {
      const doc = await fetchPage(link.href);
      const list = cards();
      for (const li of [...doc.querySelector("ul.cards").children]) {
        if (!document.getElementById(li.id)) list.append(li);
      }
      swapMore(doc);
    } catch {
      // The link still works, and the next scroll tries again.
    } finally {
      loadingMore = false;
    }
  }

  const seen = new IntersectionObserver((entries) => {
    if (entries.some((e) => e.isIntersecting)) loadMore();
  }, { rootMargin: "600px" });

  function watchMore() {
    seen.disconnect();
    if (more()) seen.observe(more());
  }

  async function poll() {
    if (document.hidden) return;
    try {
      const res = await fetch("/changes");
      if (res.redirected) {
        location.href = "/login";
        return;
      }
      const { version: now } = await res.json();
      if (version !== null && now !== version) await refresh();
      version = now;
    } catch {
      // Offline for a moment; the next poll catches up.
    }
  }

  document.addEventListener("click", (e) => {
    if (e.target === more()) {
      e.preventDefault();
      loadMore();
    }
    if (e.target.matches("[data-tick-all]")) {
      const boxes = [...e.target.closest("section").querySelectorAll("input[type=checkbox]")];
      const on = boxes.some((b) => !b.checked);
      boxes.forEach((b) => { b.checked = on; });
    }
  });
  document.addEventListener("change", () => {
    if (held && !liveArea()?.querySelector("input[type=checkbox][form]:checked")) {
      held = false;
      refresh().catch(() => {});
    }
  });
  document.addEventListener("visibilitychange", poll);
  watchMore();
  poll();
  setInterval(poll, pollEvery);
})();
