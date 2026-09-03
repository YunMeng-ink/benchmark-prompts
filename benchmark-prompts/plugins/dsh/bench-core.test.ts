/**
 * bench-core.test.ts —— dsh 侧 bench-core 副本的独立回归（不依赖 DSH 安装树）。
 *
 * 深度逻辑在 plugins/pi/extension/bench-core.test.ts（31 项）里已经钉死；
 * 本文件的职责是"漂移防线"：证明 dsh 这份与 pi 那份逐字节相同，并对关键
 * 导出做冒烟行为断言（万一两边都坏，行为断言还能兜住）。
 *
 * 运行：node --test plugins/dsh/bench-core.test.ts
 */
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import {
	assertId,
	assertScoreValue,
	type BenchError,
	buildArgs,
	decodeBench,
	ExitCode,
	exitCodeLabel,
	formatStatus,
	resolveBinary,
	truncate,
} from "./bench-core.ts";

const HERE = dirname(fileURLToPath(import.meta.url));
const BENCH_CORE_SHA256 = "2abb6536c7487bcf569e550c8284adaa5f83d662fa155e3296cd41dbc7c473cf";

test("dsh 副本与钉住哈希一致（与 pi 侧同步修改后才许更新钉值）", () => {
	const hash = createHash("sha256")
		.update(readFileSync(join(HERE, "bench-core.ts")))
		.digest("hex");
	assert.equal(hash, BENCH_CORE_SHA256);
});

test("仓库布局可用时逐字节等于 pi 侧副本", () => {
	const piCopy = join(HERE, "..", "pi", "extension", "bench-core.ts");
	if (!existsSync(piCopy)) return;
	assert.deepEqual([...readFileSync(join(HERE, "bench-core.ts"))], [...readFileSync(piCopy)]);
});

test("buildArgs：选项在前位置参数在后，--json 恒在", () => {
	assert.deepEqual(buildArgs("random", { tag: "x", fresh: true }), ["random", "--json", "--tag", "x", "--fresh"]);
	assert.deepEqual(buildArgs("get", { id: "p_1", local: true }), ["get", "--json", "--local", "p_1"]);
	assert.deepEqual(buildArgs("score", { id: "p_1", value: 3 }), ["score", "--json", "p_1", "3"]);
	assert.deepEqual(buildArgs("upload", { content: "c", tags: ["a", "b"] }), [
		"upload",
		"--json",
		"-c",
		"c",
		"-t",
		"a,b",
	]);
	assert.deepEqual(buildArgs("sync"), ["sync", "--json"]);
});

test("入参前置校验", () => {
	assert.equal(assertScoreValue(4), 4);
	assert.throws(() => assertScoreValue(3.5), /1-5/);
	assert.throws(() => assertId("  ", "bench get"), /非空/);
});

test("decodeBench：成功取 data；退出码映射；坏响应识别", () => {
	assert.deepEqual(
		decodeBench({ stdout: '{"data":{"a":1},"error":null,"ok":true,"v":1}', stderr: "", code: 0 }, "b", ["meta"]),
		{ a: 1 },
	);
	assert.throws(
		() =>
			decodeBench(
				{ stdout: '{"data":null,"error":{"code":"not_found","message":"m"},"ok":false,"v":1}', stderr: "", code: 4 },
				"b",
				["get"],
			),
		(err: BenchError) => err.code === "not_found" && !err.retryable,
	);
	assert.throws(() => decodeBench({ stdout: "not json", stderr: "", code: 0 }, "b", ["meta"]), /JSON|版本过旧/);
	assert.equal(exitCodeLabel(ExitCode.RATE_LIMITED), "被限流，稍后重试");
	assert.throws(
		() =>
			decodeBench(
				{
					stdout: '{"data":null,"error":{"code":"network","message":"x"},"ok":false,"v":1}',
					stderr: "",
					code: 1,
					killed: true,
				},
				"b",
				["meta"],
			),
		(err: BenchError) => err.code === "canceled" && err.retryable,
	);
});

test("truncate 处理单行超长不空手而归", () => {
	const one = "あ".repeat(20_000); // 每字符 3 字节
	const r = truncate(one, 120, 1_000);
	assert.ok(r.truncated);
	assert.ok(r.text.length > 0);
	assert.ok(Buffer.byteLength(r.text, "utf8") <= 1_000);
});

test("resolveBinary 与 formatStatus", () => {
	assert.deepEqual(resolveBinary({ BENCH_BIN: " my/bench " } as never), { bin: "my/bench", source: "BENCH_BIN" });
	assert.equal(resolveBinary({} as never).bin, "bench");
	assert.equal(formatStatus({ server_total: 2, local_count: 2, up_to_date: true }), "服务端 2 条 / 本地 2 条 / 已同步");
});
