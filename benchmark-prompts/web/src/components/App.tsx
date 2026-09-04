import { useEffect, useState } from "preact/hooks";

import { current, type Route, subscribe } from "../lib/router";
import Credentials from "./Credentials";
import DetailView from "./DetailView";
import PromptList from "./PromptList";
import RandomView from "./RandomView";
import UploadView from "./UploadView";

const NAV: Array<{ href: string; label: string; match: Route["name"] }> = [
	{ href: "#/", label: "列表", match: "list" },
	{ href: "#/random", label: "随机一条", match: "random" },
	{ href: "#/upload", label: "上传", match: "upload" },
];

/** 根岛：hash 路由 + 四个视图。视图内部各自负责取数与错误提示。 */
export default function App() {
	const [now, setNow] = useState<Route>(current());

	useEffect(() => subscribe(setNow), []);

	return (
		<>
			<nav class="nav" aria-label="主导航">
				{NAV.map((n) => (
					<a key={n.href} href={n.href} aria-current={now.name === n.match ? "page" : undefined}>
						{n.label}
					</a>
				))}
			</nav>

			<main>
				{now.name === "list" && <PromptList />}
				{now.name === "random" && <RandomView />}
				{now.name === "upload" && <UploadView />}
				{now.name === "detail" && <DetailView id={now.id} />}
			</main>

			<footer>
				<Credentials />
			</footer>
		</>
	);
}
