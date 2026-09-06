import assert from 'node:assert/strict';
import { test } from 'node:test';
import { addressCount, collapse, parseAddress, summarize } from './cdn-summarize.mjs';

const V4OPT = { pad: false, pad4: 24, pad6: 64 };
const run = (lines, opt = V4OPT) => summarize(lines.join('\n'), opt);
const net = (v, p) => ({ value: BigInt(v), prefix: p, bits: 32 });
const num = (s) =>
	s.split('.').reduce((a, b) => (a << 8n) | BigInt(b), 0n) & ((1n << 32n) - 1n);

function toText(blocks) {
	return blocks.map((b) => (b.bits === 32 ? `${dotted(b.net)}/${b.prefix}` : `[v6]${b.net}/${b.prefix}`));
}
const dotted = (v) => [3, 2, 1, 0].map((i) => Number((v >> BigInt(i * 8)) & 0xffn)).join('.');

// ── 地址解析 ────────────────────────────────────────────────────
test('parseAddress 接受 IPv4 与各种 IPv6 写法', () => {
	assert.deepEqual(parseAddress('178.236.38.77'), { bits: 32, value: num('178.236.38.77') });
	assert.equal(parseAddress(' ::1 ').bits, 128);
	assert.equal(parseAddress('2404:8c80:85:1100::19').bits, 128);
	assert.equal(parseAddress('2001:0db8:0000:0000:0000:0000:0000:0001').value, parseAddress('2001:db8::1').value);
	assert.equal(parseAddress('::').value, 0n);
	// 末尾 IPv4 形式必须等价于展开后的组
	assert.equal(parseAddress('::ffff:1.2.3.4').value, parseAddress('::ffff:0102:0304').value);
});

test('parseAddress 拒绝非法输入而不是悄悄丢掉', () => {
	// 每一项都必须是拒绝，而不是“返回个奇怪的值继续跑”
	for (const bad of ['x', '256.0.0.1', '1.2.3', '1.2.3.4.5', '1.2.3.4x', '1.2.3.4/24', ':::', '::::', 'g::1', '2001:db8:::1', 'fe80::1%eth0']) {
		assert.equal(parseAddress(bad), null, `应拒绝 ${JSON.stringify(bad)}`);
	}
	assert.equal(parseAddress(''), null);
	assert.deepEqual(parseAddress('  203.0.113.5  '), { bits: 32, value: num('203.0.113.5') }); // 前后空白要能吃掉
});

// ── 精确覆盖 ────────────────────────────────────────────────────
test('两个相邻地址并成 /31，且不多覆盖任何地址', () => {
	const r = run(['10.0.0.0', '10.0.0.1']);
	assert.equal(r.blocks4.length, 1);
	assert.equal(r.blocks4[0].prefix, 31);
	assert.equal(dotted(r.blocks4[0].net), '10.0.0.0');
	assert.equal(addressCount(r.blocks4), 2n);
});

test('未对齐的一对不合并（10.0.0.1 + 10.0.0.2 不是兄弟块）', () => {
	const r = run(['10.0.0.1', '10.0.0.2']);
	assert.equal(r.blocks4.length, 2);
	assert.equal(addressCount(r.blocks4), 2n);
});

test('两个满 /24 并成 /23 —— 真实清单里 178.236.38/39 就是这个形状', () => {
	const ips = [];
	for (let i = 0; i < 256; i++) ips.push(`178.236.38.${i}`);
	for (let i = 0; i < 256; i++) ips.push(`178.236.39.${i}`);
	const r = run(ips);
	assert.equal(r.blocks4.length, 1, '应归约为一条 /23');
	assert.equal(r.blocks4[0].prefix, 23);
	assert.equal(dotted(r.blocks4[0].net), '178.236.38.0');
	assert.equal(addressCount(r.blocks4), 512n);
});

test('顺序无关，且重复输入不改变结果', () => {
	const a = run(['1.1.1.1', '2.2.2.2', '10.0.0.0', '10.0.0.1']);
	const b = run(['10.0.0.1', '2.2.2.2', '1.1.1.1', '10.0.0.0', '1.1.1.1']);
	assert.deepEqual(toText(a.blocks4), toText(b.blocks4));
});

test('精确覆盖的不变式：覆盖地址数 == 输入去重数，且每个输入都在网段内', () => {
	const ips = [];
	for (let i = 0; i < 900; i++) {
		// 伪随机但确定的地址，制造大量碎片与小段
		const o = (i * 2654435761) % 65536;
		ips.push(`20.${(i >> 3) & 0xff}.${o >> 8}.${o & 0xff}`);
	}
	const uniq = new Set(ips);
	const r = run(ips);
	assert.equal(addressCount(r.blocks4), BigInt(uniq.size), '精确覆盖不允许宽化');
	const covered = r.blocks4.map((b) => ({ net: b.net, prefix: b.prefix, span: 1n << BigInt(32 - b.prefix) }));
	for (const s of uniq) {
		const v = num(s);
		assert.ok(covered.some((c) => v >= c.net && v - c.net < c.span), `${s} 未被覆盖`);
	}
});

test('IPv4 与 IPv6 分别归约，绝不互相污染', () => {
	const r = run(['203.0.113.5', '2404:8c80:85:1100::18', '2404:8c80:85:1100::19']);
	assert.equal(r.blocks4.length, 1);
	assert.equal(r.blocks6.length, 1);
	assert.equal(r.blocks6[0].prefix, 127, '::18 与 ::19 是 /127 的兄弟对');

	// :19 与 :20 不是兄弟（奇数/偶数不能逆序配对），必须保持两条
	const r2 = run(['2404:8c80:85:1100::19', '2404:8c80:85:1100::20']);
	assert.equal(r2.blocks6.length, 2);
	assert.equal(addressCount(r2.blocks6), 2n);
});

// ── 宽化模式 ────────────────────────────────────────────────────
test('--pad 用整段换取轮换容忍，并如实报出信任扩张', () => {
	const ips = ['103.112.173.7', '103.112.173.8'];
	const exact = run(ips);
	const padded = run(ips, { pad: true, pad4: 24, pad6: 64 });
	assert.equal(addressCount(exact.blocks4), 2n);
	assert.equal(addressCount(padded.blocks4), 256n, '按 /24 宽化后应覆盖整段');
	assert.equal(padded.blocks4[0].prefix, 24);
	assert.equal(dotted(padded.blocks4[0].net), '103.112.173.0');
});

test('CRLF 与行尾注释都被容忍（清单常从 Windows 导出）', () => {
	const r = summarize('# 导出于控制台\r\n203.0.113.5\r\n\r\n203.0.113.6\r\n', V4OPT);
	assert.equal(r.bad.length, 0);
	assert.equal(addressCount(r.blocks4), 2n);
});

test('非法行必须报出来，不能静默少配一段网段', () => {
	const r = run(['203.0.113.5', 'nginx.example.com', '999.1.1.1']);
	assert.deepEqual(r.bad, ['nginx.example.com', '999.1.1.1']);
});

test('collapse 直接调用：给定 CIDR 集合也能合并', () => {
	const blocks = collapse([net(0x0a000000, 24), net(0x0a000100, 24)], 32);
	assert.equal(blocks.length, 1);
	assert.equal(blocks[0].prefix, 23);
});
