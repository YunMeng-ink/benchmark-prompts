// Package cli 实现 bench 命令行的全部逻辑。
//
// 为什么不直接写在 cmd/cli：main 无法被单测驱动。这里对外只暴露
//
//	Run(ctx, args) int
//
// cmd/cli 只是三行胶水，于是每个子命令都能被测试直接调用并断言输出与退出码。
//
// 输出约定（供 DSH / Pi 插件解析，见 docs/plugins.md）：
//   - 默认：人类可读文本走 stdout，元信息/错误走 stderr
//   - --json：结构化结果走 stdout，错误也输出信封 JSON 到 stdout
//   - 退出码：见 usageText 末尾
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/benchmark-prompts/internal/buildinfo"
	"github.com/example/benchmark-prompts/pkg/client"
)

// Version 是构建注入的版本号，转发到 internal/buildinfo。
// 保留这个别名是因为 UserAgent 需要它，且旧文档里的注入点写着本包路径；
// 新代码请直接读 buildinfo.Version。
var Version = buildinfo.Version

// App 是一次 CLI 运行的上下文。
type App struct {
	out    io.Writer
	errOut io.Writer
	// dial 便于测试替换客户端构造
	dial func(client.Options) (*client.Client, error)
}

// New 构造 App。out/errOut 传 os.Stdout / os.Stderr 即为真实运行。
func New(out, errOut io.Writer) *App {
	return &App{out: out, errOut: errOut, dial: client.New}
}

// globals 是扁平的参数集合：全部子命令的参数都注册在同一 FlagSet 上。
//
// 为什么不分层：stdlib flag 必须在 Parse() 之前注册完所有 flag，
// 而子命令逻辑是在 Parse() 之后才跑的。要么维护“命令→参数集”映射，
// 要么扁平化。这里选后者：代码量少很多，代价是不拦截与命令无关的参数。
// 对只被插件调用的工具来说，这个代价可接受。
type globals struct {
	endpoint string
	key      string
	secret   string
	home     string
	asJSON   bool
	quiet    bool
	timeout  time.Duration

	// random / list
	tag     string
	exclude string
	limit   int
	all     bool
	fresh   bool

	// key new
	invite string
	label  string

	// upload
	content  string
	file     string
	tags     string
	clientID string

	// get
	local bool
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.endpoint, "endpoint", "", "服务地址，覆盖配置文件")
	fs.StringVar(&g.key, "key", "", "API Key，覆盖配置文件")
	fs.StringVar(&g.secret, "secret", "", "HMAC secret，非空时走签名鉴权")
	fs.StringVar(&g.home, "home", "", "配置与缓存目录，等价于 BENCH_HOME")
	fs.BoolVar(&g.asJSON, "json", false, "输出结构化 JSON（插件请用它）")
	fs.BoolVar(&g.quiet, "quiet", false, "抑制 stderr 上的元信息")
	fs.DurationVar(&g.timeout, "timeout", 20*time.Second, "单次命令总超时")

	fs.StringVar(&g.tag, "tag", "", "按标签过滤")
	fs.StringVar(&g.exclude, "exclude", "", "逗隔的 id 列表，不抽到它们")
	fs.IntVar(&g.limit, "limit", 0, "每页条数（0=用默认）")
	fs.BoolVar(&g.all, "all", false, "自动翻页直到结束")
	fs.BoolVar(&g.fresh, "fresh", false, "random 排除最近已抽过的条目（而不是全部缓存）")

	fs.StringVar(&g.invite, "invite", "", "key new 用的邀请码")
	fs.StringVar(&g.label, "label", "", "key new 给这台设备起的备注名")

	fs.StringVar(&g.content, "c", "", "upload 的提示词正文")
	fs.StringVar(&g.content, "content", "", "upload 的提示词正文")
	fs.StringVar(&g.file, "f", "", "upload 从一个文件读正文")
	fs.StringVar(&g.file, "file", "", "upload 从一个文件读正文")
	fs.StringVar(&g.tags, "t", "", "upload 的标签，逗号分隔")
	fs.StringVar(&g.tags, "tags", "", "upload 的标签，逗号分隔")
	fs.StringVar(&g.clientID, "client-id", "", "upload 的幂等键（断网重放用）")

	fs.BoolVar(&g.local, "local", false, "get 只读本地缓存，不访问网络")
}

const usageText = `bench —— 个人测试用 benchmark 提示词客户端

用法：
  bench <命令> [全局参数]

命令：
  meta                        显示服务端与本地缓存状态
  sync                        增量同步到本地缓存
  get <id>                    取一条提示词（一键测试）
  random [--tag=] [--fresh]   随机取一条（随机测试）
  list [--tag=] [--limit=]    浏览目录摘要（不含正文，省带宽）
  score <id> <1-5>            为提示词打分
  upload (-c 文本 | -f 文件)  上传新提示词，进入审核队列
  key new|self|revoke         邀请码自助注册 Key / 查看 / 吊销自己这把
  config init|show|set        管理本地配置
  reset                       清空本地缓存（保留 device_id）
  version                     显示版本

全局参数：
  --endpoint URL    覆盖服务地址
  --key / --secret  覆盖凭据
  --home DIR        覆盖配置与缓存目录
  --json            输出结构化 JSON
  --quiet           抑制 stderr 元信息
  --timeout DUR     总超时，默认 20s

环境变量：BENCH_HOME / BENCH_ENDPOINT / BENCH_API_KEY / BENCH_SECRET / BENCH_DEVICE_ID

退出码：
  0 成功   1 网络或服务端故障   2 被限流（可稍后重试）
  3 鉴权   4 资源不存在         5 参数或校验错误
`

// Run 执行一次命令，返回进程退出码。
func (a *App) Run(ctx context.Context, args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}

	switch name {
	case "help", "-h", "--help":
		_, _ = io.WriteString(a.out, usageText)
		return client.ExitOK
	case "":
		_, _ = io.WriteString(a.errOut, usageText)
		return client.ExitBadInput
	}

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	g := &globals{}
	g.register(fs)

	// flag 包要求选项必须在位置参数之前，否则遇到第一个位置就停止解析。
	// 先把两者分开，才能支持 `bench config init --home X` 这种自然写法。
	flagArgs, rest := splitArgs(args[1:])
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return client.ExitOK
		}
		return client.ExitBadInput
	}

	if g.timeout > 0 {
		tctx, cancel := context.WithTimeout(ctx, g.timeout)
		defer cancel()
		ctx = tctx
	}

	var err error
	switch name {
	case "meta":
		err = a.cmdMeta(ctx, g)
	case "sync":
		err = a.cmdSync(ctx, g)
	case "get":
		err = a.cmdGet(ctx, g, rest)
	case "random":
		err = a.cmdRandom(ctx, g, rest)
	case "list":
		err = a.cmdList(ctx, g)
	case "score":
		err = a.cmdScore(ctx, g, rest)
	case "upload":
		err = a.cmdUpload(ctx, g)
	case "key":
		err = a.cmdKey(ctx, g, rest)
	case "config":
		err = a.cmdConfig(ctx, g, rest)
	case "reset":
		err = a.cmdReset(ctx, g)
	case "version":
		err = a.cmdVersion(g)
	default:
		a.errorf(g, "未知命令 %q\n", name)
		_, _ = io.WriteString(a.errOut, usageText)
		return client.ExitBadInput
	}

	if err != nil {
		return a.reportErr(g, err)
	}
	return client.ExitOK
}

// reportErr 输出错误并给出退出码。--json 时错误也走 stdout 的结构化信封，
// 这样插件可以无脑 `bench ... --json` 然后解析，不必去混流的 stderr 里找原因。
func (a *App) reportErr(g *globals, err error) int {
	code, exit := classifyError(err)

	if g.asJSON {
		_ = json.NewEncoder(a.out).Encode(map[string]any{
			"ok":    false,
			"data":  nil,
			"error": map[string]any{"code": code, "message": err.Error()},
			"v":     1,
		})
		return exit
	}
	_, _ = fmt.Fprintf(a.errOut, "错误[%s] %v\n", code, err)
	return exit
}

func classifyError(err error) (code string, exit int) {
	// client 层已把所有服务端错误与解码失败统一包装成 *client.Error，
	// 因此这里只需要兵分两路：已分类的走它自己的码，其余归本地错误。
	var ce *client.Error
	if errors.As(err, &ce) {
		return ce.Code, ce.ExitCode()
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return client.CodeUnavailable, client.ExitNetwork
	case errors.Is(err, context.Canceled):
		return "canceled", client.ExitNetwork
	default:
		return "local_error", client.ExitBadInput
	}
}

// ---- 客户端与输出辅助 ----

func (g *globals) configPath() (string, error) {
	if g.home != "" {
		return filepath.Join(g.home, "config"), nil
	}
	return client.DefaultConfigPath()
}

func (g *globals) cachePath() (string, error) {
	if g.home != "" {
		return filepath.Join(g.home, "cache.db"), nil
	}
	return client.DefaultCachePath()
}

// loadConfig 读取"配置文件 + 环境变量 + 命令行"三层合并结果。
func (g *globals) loadConfig() (*client.Config, error) {
	path, err := g.configPath()
	if err != nil {
		return nil, err
	}
	cfg, err := client.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	cfg.ApplyEnv()
	if g.endpoint != "" {
		cfg.Endpoint = g.endpoint
	}
	if g.key != "" {
		cfg.APIKey = g.key
	}
	if g.secret != "" {
		cfg.Secret = g.secret
	}
	return cfg, nil
}

// clientFor 构造客户端；未配置 endpoint 时给出可直接照做的提示。
func (a *App) clientFor(g *globals) (*client.Client, error) {
	cfg, err := g.loadConfig()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("尚未配置服务地址；请运行 bench config init --endpoint <url>，或设置环境变量 BENCH_ENDPOINT")
	}
	ep, err := client.NormalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	cfg.Endpoint = ep

	opt := cfg.ToOptions()
	opt.UserAgent = "bench-cli/" + Version
	opt.Timeout = g.timeout
	opt.CachePath, err = g.cachePath()
	if err != nil {
		return nil, err
	}
	return a.dial(opt)
}

// emit 输出一个结果对象：--json 走**与服务端一致的信封**，否则走人类可读渲染。
//
// 为什么成功也要包信封：早期版本成功时输出裸对象、失败时输出信封，
// 插件就得写两套解析分支，而且很容易把"缺 ok 字段"当成成功。
// 统一成 {ok,data,error,v} 后，无论成败都是同一个形状。
func (a *App) emit(g *globals, v any, human func()) error {
	if g.asJSON {
		return json.NewEncoder(a.out).Encode(map[string]any{
			"ok": true, "data": v, "error": nil, "v": 1,
		})
	}
	human()
	return nil
}

func (a *App) notef(g *globals, format string, args ...any) {
	if g.quiet {
		return
	}
	_, _ = fmt.Fprintf(a.errOut, format, args...)
}

func (a *App) errorf(g *globals, format string, args ...any) {
	if g.asJSON {
		return // 由 reportErr 统一输出，避免混流
	}
	_, _ = fmt.Fprintf(a.errOut, format, args...)
}

func writeLine(w io.Writer, s string) { _, _ = io.WriteString(w, s+"\n") }

// boolFlags 是不带取值的开关，用于参数重排时判断下一个 token 是否属于本 flag。
var boolFlags = map[string]bool{
	"json": true, "quiet": true, "all": true, "fresh": true, "local": true,
	"h": true, "help": true,
}

// splitArgs 把选项与位置参数分开。
//
// Go 的 flag 包一旦遇到第一个非选项参数就停止解析，直接把
// ["init", "--home", "/tmp/x"] 喂进去会让 --home 被当成位置参数而**静默失效**
// （不报错、不提示），对用户和插件都很难排查。这里先做一次分拢。
func splitArgs(args []string) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]

		if a == "--" { // 显式终止选项解析
			positional = append(positional, args[i+1:]...)
			return flagArgs, positional
		}
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}

		name := strings.TrimLeft(a, "-")
		switch {
		case strings.Contains(name, "="): // --limit=10 形式，自带值
			flagArgs = append(flagArgs, a)
		case boolFlags[name]:
			flagArgs = append(flagArgs, a)
		case i+1 < len(args): // 需要取值的 flag，把下一个 token 一起带上
			flagArgs = append(flagArgs, a, args[i+1])
			i++
		default: // 缺值，交给 flag 包报 "flag needs an argument"
			flagArgs = append(flagArgs, a)
		}
	}
	return flagArgs, positional
}
