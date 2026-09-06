#!/usr/bin/env node
// cdn-summarize.mjs —— 把 CDN 回源节点的单 IP 列表归约成 CIDR 网段集，
// 供源站 `server.trusted_proxies` 使用（见 docs/deployment.md §2.4）。
//
// 两种语义，差别是**信任边界**，必须自己选：
//   默认（精确覆盖）：只覆盖列表里出现过的地址，逐层合并兄弟块。
//                     不信任任何未列出的地址；CDN 换节点就要重新生成。
//   --pad           ：先把每个地址取整到 --pad4 / --pad6（默认 /24、/64）再合并。
//                     容忍节点轮换，代价是顺带信任同段内未列出的地址 ——
//                     稀疏段里这个差值可能很大，脚本会把它报出来。
//
// 用法：
//   node scripts/cdn-summarize.mjs CDNnode.txt
//   node scripts/cdn-summarize.mjs CDNnode.txt --pad
//   node scripts/cdn-summarize.mjs CDNnode.txt --yaml      # 直接可贴进配置
//
// 退出码 0 正常；1 输入含非法地址（不静默丢弃，配错网段等于把限流敞开）。

import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

/** IPv4/IPv6 文本 → {bits, value}；非法返回 null。 */
export function parseAddress(text) {
	const s = String(text ?? '').trim();
	if (!s || s.includes('%') || s.includes('/')) return null;
	if (s.includes(':')) {
		const v = parseV6(s);
		return v === null ? null : { bits: 128, value: v };
	}
	const parts = s.split('.');
	if (parts.length !== 4) return null;
	let v = 0n;
	for (const p of parts) {
		if (!/^\d{1,3}$/.test(p)) return null;
		const n = Number(p);
		if (n > 255) return null;
		v = (v << 8n) | BigInt(n);
	}
	return { bits: 32, value: v };
}

/** IPv6 文本 → BigInt。支持 :: 缩写与末尾 IPv4（::ffff:1.2.3.4）。 */
function parseV6(s) {
	let head = s;
	if (head.includes('.')) {
		// 把末尾的 IPv4 段换成两个十六进制组，剩下的都是纯 v6 组。
		const i = head.lastIndexOf(':');
		const v4 = parseAddress(head.slice(i + 1));
		if (!v4) return null;
		head = `${head.slice(0, i)}:${((v4.value >> 16n) & 0xffffn).toString(16)}:${(v4.value & 0xffffn).toString(16)}`;
	}
	const isHex = (g) => /^[0-9a-fA-F]{1,4}$/.test(g);
	const sep = head.indexOf('::');
	let left;
	let right;
	if (sep >= 0) {
		if (head.indexOf('::', sep + 1) >= 0) return null; // 只允许一个 ::
		const ls = head.slice(0, sep);
		const rs = head.slice(sep + 2);
		left = ls === '' ? [] : ls.split(':');
		right = rs === '' ? [] : rs.split(':');
	} else {
		left = head.split(':');
		right = [];
		if (left.length !== 8) return null;
	}
	const words = [...left, ...right];
	if (words.some((g) => !isHex(g))) return null;
	const nums = words.map((g) => parseInt(g, 16));
	if (sep >= 0) {
		const gap = 8 - nums.length;
		if (gap < 1) return null; // :: 至少代表一组全零（除 :: 本身表示 8 组零）
		nums.splice(left.length, 0, ...new Array(gap).fill(0));
	}
	if (nums.length !== 8) return null;
	let v = 0n;
	for (const g of nums) v = (v << 16n) | BigInt(g);
	return v;
}

const mask = (bits, prefix) => {
	if (prefix <= 0) return 0n;
	if (prefix >= bits) return (1n << BigInt(bits)) - 1n;
	return ((1n << BigInt(prefix)) - 1n) << BigInt(bits - prefix);
};

/**
 * 精确覆盖：从给定 (value, prefix) 集合出发，逐层把“同父的两个相邻子块”并成父块。
 * 结果与地址顺序无关；不会产生覆盖多余地址的块。
 */
export function collapse(entries, bits) {
	// key: `prefix:网络号`
	const set = new Map();
	for (const e of entries) {
		const net = e.value & mask(bits, e.prefix);
		set.set(`${e.prefix}:${net}`, { net, prefix: e.prefix });
	}
	for (let prefix = bits; prefix >= 1; prefix--) {
		const span = 1n << BigInt(bits - prefix); // 一个子块的地址数
		const parents = new Map();
		for (const item of set.values()) {
			if (item.prefix !== prefix) continue;
			const parent = item.net & mask(bits, prefix - 1);
			if (!parents.has(parent)) parents.set(parent, []);
			parents.get(parent).push(item.net);
		}
		for (const [parent, children] of parents) {
			if (children.length !== 2) continue;
			const [a, b] = [...children].sort((x, y) => (x < y ? -1 : 1));
			// 必须正好是父块的左半与右半，不能重叠也不能缺。
			if (a === parent && b === parent + span) {
				set.delete(`${prefix}:${a}`);
				set.delete(`${prefix}:${b}`);
				set.set(`${prefix - 1}:${parent}`, { net: parent, prefix: prefix - 1 });
			}
		}
	}
	return [...set.values()].sort(
		(x, y) => x.prefix - y.prefix || (x.net < y.net ? -1 : x.net > y.net ? 1 : 0),
	);
}

const fmt4 = (v) => [3, 2, 1, 0].map((i) => Number((v >> BigInt(i * 8)) & 0xffn)).join('.');

function fmt6(v) {
	const g = [];
	for (let i = 7; i >= 0; i--) g.push(Number((v >> BigInt(i * 16)) & 0xffffn).toString(16));
	let best = -1;
	let bestLen = 0;
	for (let i = 0; i < 8; i++) {
		if (g[i] !== '0') continue;
		let j = i;
		while (j < 8 && g[j] === '0') j++;
		if (j - i > bestLen) {
			bestLen = j - i;
			best = i;
		}
	}
	if (bestLen >= 2) return `${g.slice(0, best).join(':')}::${g.slice(best + bestLen).join(':')}`;
	return g.join(':');
}

export function format(block) {
	const s = block.bits === 32 ? fmt4(block.net) : fmt6(block.net);
	return `${s}/${block.prefix}`;
}

/** 统计网段集合覆盖的地址总数。 */
export function addressCount(blocks) {
	return blocks.reduce((acc, b) => acc + (1n << BigInt(b.bits - b.prefix)), 0n);
}

export function parseArgs(argv) {
	const opt = { file: null, pad: false, pad4: 24, pad6: 64, yaml: false };
	const need = (name, v) => {
		const n = Number(v);
		if (!Number.isInteger(n)) throw new Error(`${name} 需要一个整数`);
		return n;
	};
	for (let i = 0; i < argv.length; i++) {
		const a = argv[i];
		if (a === '--pad') opt.pad = true;
		else if (a === '--pad4') opt.pad4 = need(a, argv[++i]);
		else if (a === '--pad6') opt.pad6 = need(a, argv[++i]);
		else if (a === '--yaml') opt.yaml = true;
		else if (!opt.file) opt.file = a;
		else throw new Error(`多余的位置参数：${a}`);
	}
	if (!opt.file) {
		throw new Error('用法：cdn-summarize.mjs <IP 列表文件> [--pad] [--pad4 N] [--pad6 N] [--yaml]');
	}
	if (opt.pad4 < 0 || opt.pad4 > 32) throw new Error('--pad4 必须在 0..32');
	if (opt.pad6 < 0 || opt.pad6 > 128) throw new Error('--pad6 必须在 0..128');
	return opt;
}

export function summarize(text, opt) {
	const bad = [];
	const v4 = [];
	const v6 = [];
	for (const raw of text.replace(/\r\n/g, '\n').split('\n')) {
		const line = raw.trim();
		if (!line || line.startsWith('#')) continue;
		const p = parseAddress(line);
		if (!p) {
			bad.push(line);
			continue;
		}
		const prefix = opt.pad ? (p.bits === 32 ? opt.pad4 : opt.pad6) : p.bits;
		(p.bits === 32 ? v4 : v6).push({ value: p.value, prefix, bits: p.bits });
	}
	const build = (arr, bits) => collapse(arr, bits).map((b) => ({ ...b, bits }));
	return {
		blocks4: build(v4, 32),
		blocks6: build(v6, 128),
		input4: v4.length,
		input6: v6.length,
		bad,
	};
}

function main() {
	const opt = parseArgs(process.argv.slice(2));
	const r = summarize(readFileSync(opt.file, 'utf8'), opt);
	if (r.bad.length) {
		console.error(`输入含 ${r.bad.length} 条非法地址，未静默丢弃。前 5 条：`);
		for (const b of r.bad.slice(0, 5)) console.error(`  ${b}`);
		process.exit(1);
	}
	const total = r.input4 + r.input6;
	const blocks = [...r.blocks4, ...r.blocks6];
	const covered = addressCount(r.blocks4) + addressCount(r.blocks6);
	const lines = [
		`# 来源 ${opt.file}：${total} 个地址（IPv4 ${r.input4} / IPv6 ${r.input6}）`,
		`# 语义：${opt.pad ? `先按 /${opt.pad4} 与 /${opt.pad6} 整段宽化再合并` : '精确覆盖'}`,
		`# 归约：${total} 个地址 → ${blocks.length} 条网段，合计覆盖 ${covered} 个地址` +
			(covered > BigInt(total) ? `（宽化多出 ${covered - BigInt(total)} 个未列出的地址，属信任扩张）` : ''),
	];
	if (opt.yaml) {
		lines.push('trusted_proxies:');
		lines.push('  - "127.0.0.0/8"   # 同机 nginx，必须保留；填写列表是整体替换');
		lines.push('  - "::1/128"');
		for (const b of blocks) lines.push(`  - "${format(b)}"`);
	} else {
		for (const b of blocks) lines.push(format(b));
	}
	console.log(lines.join('\n'));
}

if (process.argv[1] && pathToFileURL(process.argv[1]).href === import.meta.url) {
	try {
		main();
	} catch (e) {
		console.error(String(e.message ?? e));
		process.exit(1);
	}
}
