/** hash 路由（docs/frontend.md §2）：CDN 不需要任何重写规则。 */

export type Route = { name: "list" } | { name: "random" } | { name: "detail"; id: string } | { name: "upload" };

export function parse(hash: string): Route {
	const seg = hash.replace(/^#\/?/, "").split("?")[0];
	if (seg === "") return { name: "list" };
	if (seg === "random") return { name: "random" };
	if (seg === "upload") return { name: "upload" };
	if (seg.startsWith("detail/")) {
		const id = decodeURIComponent(seg.slice("detail/".length));
		if (id) return { name: "detail", id };
	}
	return { name: "list" };
}

export function current(): Route {
	return parse(globalThis.location?.hash ?? "");
}

export function go(path: string): void {
	if (globalThis.location) globalThis.location.hash = path.startsWith("#") ? path : `#${path}`;
}

export function subscribe(cb: (route: Route) => void): () => void {
	const on = () => cb(current());
	globalThis.addEventListener?.("hashchange", on);
	return () => globalThis.removeEventListener?.("hashchange", on);
}

export function hrefFor(route: Route): string {
	switch (route.name) {
		case "random":
			return "#/random";
		case "upload":
			return "#/upload";
		case "detail":
			return `#/detail/${encodeURIComponent(route.id)}`;
		default:
			return "#/";
	}
}
