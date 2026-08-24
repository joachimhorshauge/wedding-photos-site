/* Forked from hugo-theme-gallery v4.9.3 (assets/js/lightbox.js).
 *
 * The theme's download button hands over whatever the tile links to, which for
 * this site is the 1600px copy Hugo published. The photos themselves live in
 * Backblaze, so the button here points at the full-size original in the bucket
 * (data-original) and saves it under a readable name (data-filename).
 *
 * A cross-origin `download` attribute is ignored by browsers: linking straight
 * to the bucket would open the photo instead of saving it. Fetching it as a
 * blob first works, but only if the bucket allows this origin via CORS, so we
 * check once on load and fall back to opening the file in a new tab.
 *
 * Diff against the theme file when bumping the submodule.
 */
import PhotoSwipeLightbox from "./photoswipe/photoswipe-lightbox.esm.js";
import PhotoSwipe from "./photoswipe/photoswipe.esm.js";
import PhotoSwipeDynamicCaption from "./photoswipe/photoswipe-dynamic-caption-plugin.esm.min.js";
import * as params from "@params";

const gallery = document.getElementById("gallery");

/** Whether the bucket lets this origin read photos with fetch(). */
let canSaveDirectly = false;

async function probeCors(url) {
  if (!url) return false;
  try {
    const res = await fetch(url, { method: "HEAD", mode: "cors", credentials: "omit" });
    return res.ok;
  } catch {
    return false;
  }
}

async function saveOriginal(url, filename) {
  const res = await fetch(url, { mode: "cors", credentials: "omit" });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const blobUrl = URL.createObjectURL(await res.blob());
  const a = document.createElement("a");
  a.href = blobUrl;
  a.download = filename || "";
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(blobUrl), 30000);
}

if (gallery) {
  const firstOriginal = gallery.querySelector(".gallery-item")?.dataset.original;
  probeCors(firstOriginal).then((ok) => {
    canSaveDirectly = ok;
  });

  const lightbox = new PhotoSwipeLightbox({
    gallery,
    children: ".gallery-item",
    showHideAnimationType: "zoom",
    bgOpacity: 1,
    pswpModule: PhotoSwipe,
    imageClickAction: "close",
    closeTitle: params.closeTitle,
    zoomTitle: params.zoomTitle,
    arrowPrevTitle: params.arrowPrevTitle,
    arrowNextTitle: params.arrowNextTitle,
    errorMsg: params.errorMsg,
  });

  lightbox.on("uiRegister", () => {
    lightbox.pswp.ui.registerElement({
      name: "download-button",
      order: 8,
      isButton: true,
      tagName: "a",
      html: {
        isCustomSVG: true,
        inner:
          '<path d="M20.5 14.3 17.1 18V10h-2.2v7.9l-3.4-3.6L10 16l6 6.1 6-6.1ZM23 23H9v2h14Z" id="pswp__icn-download"/>',
        outlineID: "pswp__icn-download",
      },
      onInit: (el, pswp) => {
        el.setAttribute("download", "");
        el.setAttribute("target", "_blank");
        el.setAttribute("rel", "noopener");
        el.setAttribute("title", params.downloadTitle || "Download");

        pswp.on("change", () => {
          const tile = pswp.currSlide.data.element;
          el.href = tile.dataset.original || tile.href;
          el.setAttribute("download", tile.dataset.filename || "");
        });

        el.addEventListener("click", (event) => {
          // Without CORS the fetch cannot happen, so let the browser follow
          // the link and open the photo in a new tab instead.
          if (!canSaveDirectly) return;
          event.preventDefault();
          const tile = pswp.currSlide.data.element;
          el.classList.add("is-busy");
          saveOriginal(tile.dataset.original, tile.dataset.filename)
            .catch(() => {
              canSaveDirectly = false;
              window.open(tile.dataset.original, "_blank", "noopener");
            })
            .finally(() => el.classList.remove("is-busy"));
        });
      },
    });
  });

  lightbox.on("change", () => {
    const target = lightbox.pswp.currSlide?.data?.element?.dataset["pswpTarget"];
    history.replaceState("", document.title, "#" + target);
  });

  lightbox.on("close", () => {
    history.replaceState("", document.title, window.location.pathname);
  });

  new PhotoSwipeDynamicCaption(lightbox, {
    mobileLayoutBreakpoint: 700,
    type: "auto",
    mobileCaptionOverlapRatio: 1,
  });

  lightbox.init();

  if (window.location.hash.substring(1).length > 1) {
    const target = window.location.hash.substring(1);
    const items = gallery.querySelectorAll("a");
    for (let i = 0; i < items.length; i++) {
      if (items[i].dataset["pswpTarget"] === target) {
        lightbox.loadAndOpen(i, { gallery });
        break;
      }
    }
  }
}
