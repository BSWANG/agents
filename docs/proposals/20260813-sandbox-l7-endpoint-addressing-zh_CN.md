---
title: 按 Sandbox 粒度的端点寻址
authors:
  - "@BSWANG"
reviewers:
  - "@AiRanthem"
  - "@chengzhycn"
creation-date: 2026-08-13
last-updated: 2026-08-20
status: provisional
see-also:
  - "/docs/proposals/20260813-sandbox-l7-endpoint-addressing.md"
  - "/docs/proposals/20260527-dynamic-sandbox-domain.md"
  - "/docs/proposals/20260711-short-sandbox-id.md"
---

# 按 Sandbox 粒度的端点寻址

> 本文是[英文版](https://github.com/BSWANG/agents/blob/proposal/l7-endpoint-addressing/docs/proposals/20260813-sandbox-l7-endpoint-addressing.md)
> 的中文翻译,**以英文版为准**。代码标识符、路径、配置项名和示意图内容保持英文。

## 摘要

现在要访问一个 Sandbox 只能用它的 Pod IP,所有组件都默认这个 IP 既有值又能用。这两点都不总是成立:
IP 可能是 `169.254.1.1` 这样的占位值,也可能 Pod 网络对调用方根本不通。

本提案新增 `status.endpoint`,和 `status.sandboxIp` 平级,说明某个 Sandbox 该怎么访问:`Direct`
(用 Pod IP,和现在一样)或 `Hostname`(用一个由 L7 front 提供服务的域名)。**同一个集群里这两类
可以混着存在**,这也是它必须放在对象上的原因:模式和域名每个 Sandbox 都可能不同,进程级配置无论
怎么填都是错的。

整套访问方式都放在这个字段里 —— 地址、authority、path 前缀、header,以及凭据的引用 —— 所以任何
组件都不用加新参数,两端也不可能配得不一致。

这个字段是推导出来的,不是配出来的:寻址规则本来就属于 Sandbox 跑在的那个后端,能在那个后端上把
Sandbox 创建出来的东西,自然知道它该怎么被访问。所以**不给用户留任何开关 —— 不加注解,也不加 spec
字段**。

**这一版刻意不定义谁来写。** 字段默认为空,所有 Sandbox 都是 `Direct`,直到有东西去写它:后续的
controller 扩展,或者部署方自己已经知道 front 在哪的 webhook / operator。本提案要定下来的是字段、
resolver 和消费方 —— 也就是不该在每个访问点各写一遍的那部分。

`status.endpoint` 为空就是 `Direct`,所以现有 Sandbox 和回滚后的老 controller 都照旧工作。

## 动机

- **IP 可能是占位值。** 有些部署给每个 Sandbox 都填一个固定的链路本地地址,真正的目标靠路径上的
  L7 信息来定。
- **Pod 网络可能不通。** 管理和代理 Sandbox 的组件可能在另一个集群、另一个 VPC 或另一个网络区域,
  也可能 Pod 网络只通过托管的 L7 负载均衡器对外暴露。
- **一个集群里可能两类都有**,这决定了它必须按 Sandbox 粒度做,而不能按进程做。

### 目标

- 把每个 Sandbox 怎么访问,变成对象上明确写出来、能被读到的信息。
- 让 `Direct` 和 `Hostname` 两类共存,每个访问点按各自的 Sandbox 判断。
- 不加任何参数:访问一个 Sandbox 需要的信息全在对象上。
- 不为"没有用户会做的决定"新增用户可见的 API:不加注解,也不加 spec 字段。
- 不管字段由谁写,所有部署共用同一套契约 —— 同一个字段、同一个 resolver、同一条就绪规则。
- 所有访问点共用一个 resolver,避免控制面和两个数据面各自实现、慢慢走偏。
- 什么都没声明的 Sandbox,行为不变。
- 复用现有的 `e2b-sandbox-id` / `e2b-sandbox-port`、`{port}-{sandboxID}.{domain}` 和
  `/kruise/{sandboxID}/{port}` 这几套约定。

### 非目标

- **对接外部的 Sandbox 后端**(比如把真实 E2B 集群挂在本控制面后面)。endpoint 字段是让它们将来
  能被描述出来的接口,但它们的生命周期是另一份提案的事。
- **不提供声明式的意图 API。** 没有注解、也没有 spec 字段用来选 `Hostname`。模式由 controller
  把 Sandbox 放上去的那个后端决定,而 controller 本来就知道 —— 加一个开关等于把一个没人会去选的
  决定暴露成 API,而且最终用户会在自己的 Sandbox 上看到它。
- **不包含 writer。** "哪个 Sandbox 是 `Hostname`、地址怎么渲染"不在本提案范围内。没有东西去写它,
  字段就一直为空;等某个部署真的有 front 可指了,由 controller 扩展或 status webhook 来写。
- 不负责搭 L7 front:listener、规则、证书、DNS 由部署方准备。
- 不删 `status.sandboxIp` 和 `ReturnPodIP` 扩展;不动 token 模型;不做多集群发现和反向隧道。

## 现状

有五处会把 Sandbox 解析成 `<PodIP>:<port>`:

| # | 位置 | 代码 |
|---|---|---|
| A | runtime client,明文 | `pkg/utils/runtime/runtime.go:44` |
| A' | runtime client,TLS —— authority 固定,连接强制打到 Pod IP | `client.go:283`、`:307`、`:321`、`transport.go:305` |
| B | CDP 代理 | `pkg/utils/proxyutils/default.go:40` |
| C | manager 侧 Envoy,靠 `x-envoy-original-dst-host` 走 `ORIGINAL_DST` | `pkg/proxy/ext_proc.go:139` |
| D | gateway Envoy,两个 `ORIGINAL_DST` cluster 从 `envoy.lb.original_dst` 取地址,第二个对 runtime 端口做 mTLS 重新加密 | `pkg/sandbox-gateway/filter/filter.go:146`、`:148` |

A、A'、B 是和 `agent-sandbox-controller` 共用的,controller 自己也有五处会直连 Sandbox:挂 CSI 卷和
init 握手(`controller/sandbox/core/sandbox_initializer.go:160`、`:186`)、生命周期钩子
(`lifecycle_handler.go:68`)、recycle 写文件(`recycle.go:192`)、刷 token
(`controller/securitytokenrefresh`)。所以 controller 既写这个新字段也读它,而它那几处 init 是在
Sandbox 启动过程中跑的 —— controller 访问不到的 `Hostname` Sandbox 根本初始化不完。

还有两处不是去连,而是把 Pod IP 透出去:`ReturnPodIP` 扩展
(`servers/e2b/sandbox.go:145-147`,`infra.Sandbox.GetIP()` 唯一的非测试调用方)和
`status.sandboxIp` 本身。

另有五处拿"Pod IP 非空"当就绪信号:`cache/tasks.go:128`、`infra/sandboxcr/claim.go:555`、`:926`、
`:970`,以及 `sandboxroute/route.go:98-102` —— 它把没有 Pod IP 的 Sandbox 直接算成 `creating`。
占位值会让这些判断永远成立,而 `Hostname` Sandbox 会全部过不了。

其他用到 Pod IP 的地方 —— peer gossip、控制面 Pod 之间的路由 refresh、webhook 自检 —— 不是 Sandbox
流量,不受影响。

## 设计

### 字段

```go
// Endpoint declares how this Sandbox is addressed. A nil Endpoint means
// Direct via status.podInfo.podIP.
// +optional
Endpoint *SandboxEndpoint `json:"endpoint,omitempty"`

type SandboxEndpoint struct {
    // Mode is the addressing mode: Direct or Hostname.
    Mode SandboxEndpointMode `json:"mode"`

    // Address is the host and port to connect to for this Sandbox, e.g.
    // sbx-alb.example.com:443. Required when Mode is Hostname.
    // +optional
    Address string `json:"address,omitempty"`

    // Scheme is http or https for the hop to Address. Defaults to https.
    // +optional
    Scheme string `json:"scheme,omitempty"`

    // Authority replaces the Host / :authority header, e.g.
    // {port}-sbx7f3a.sbx.example.com. Empty keeps Address as the authority.
    // +optional
    Authority string `json:"authority,omitempty"`

    // PathPrefix is prepended to the request path, e.g. /kruise/sbx7f3a/{port}.
    // +optional
    PathPrefix string `json:"pathPrefix,omitempty"`

    // Headers are sent with every request to this Sandbox, e.g.
    // {"e2b-sandbox-id": "sbx7f3a", "e2b-sandbox-port": "{port}"}.
    // +optional
    Headers map[string]string `json:"headers,omitempty"`

    // AuthSecretRef names the Secret supplying {token}. Empty means the front
    // needs no credential of its own.
    // +optional
    AuthSecretRef *corev1.LocalObjectReference `json:"authSecretRef,omitempty"`
}
```

`AuthSecretRef` 属于最终目标契约,但当前寻址 spike 暂不实现。已经验证的实现只覆盖 front 不需要额外
凭据的场景;Secret 解析、轮转和 RBAC 仍属于第 5 步。

放在 `status` 顶层而不是 `podInfo` 里:`Hostname` Sandbox 的地址跟 Pod 没关系,而且这类 Sandbox 的
`podInfo` 可能是空的。

**整套请求改写都在这个字段里,所以任何组件都不加新参数。** 只剩两个占位符,因为其余内容在写这个
字段的时候就已经确定了:`{port}` 是调用方每次自己选的,`{token}` 来自 `AuthSecretRef`。Sandbox ID、
域名和其他所有部分,在字段被写入时都已经是具体字符串,resolver 会拒绝任何它不认识的占位符。

这样做等于把传输细节写进了 Kubernetes 对象,代价是实打实的。换来三件事:配置只有一处,而不是三个
组件里各配一份同样的参数;不存在两端配得不一致的问题;以及天然支持 per-Sandbox 的 front —— 同一个
集群里的 Sandbox 可以分别用不同的地址、不同的 header 名和不同的凭据。

writer 是把字段推导出来的,而不是从哪里读意图 —— 就像今天的 `status.sandboxIp` 一样,模式由 Sandbox
跑在的那个后端决定。**这一版什么都不写**:字段保持为空、所有 Sandbox 解析成 `Direct`,先让寻址契约
落地并被消费,再谈谁去产出 `Hostname` Sandbox。已经有 L7 front 的部署可以自己写 `status.endpoint`
—— controller 扩展,或者自己的 webhook / operator —— 而这套 API、这个 resolver 和所有消费方原样继承。

这样做的结果是:有一个暂时没人写的 status 字段,这是这个形状实打实的代价。被否掉的另一种做法 ——
用注解或 spec 字段承载意图 —— 代价更大:给每个 Sandbox 加一个没有用户会去设、但最终用户又能看到的
开关,而且目前没人需要它。以后要补 writer,对所有消费方都是零影响。

最终不管由谁来写这个字段,它都欠消费方四件事:

- **只写具体值。** `{port}` 是唯一允许留到 status 里的占位符。`Validate` 会拒掉其他占位符,以及未知
  mode、`Hostname` 下缺失或格式错误的 `Address`、非法 header 名,还有最关键的一条:`{port}` 没有渲染
  到任何一个 slot —— 那样所有请求都会落到 front 默认的那个后端端口上。消费方在解析时会再校验一遍,
  未知 mode 必须失败,不能静默降级成 `Direct`。
- **一次 patch 发布。** endpoint 要和它所属的 phase、`podInfo` 在同一次 status 更新里发布,免得有
  读者把新一代的 phase 和上一代的地址配在一起。
- **每条路径都写。** 不能只在创建时写,而要在**每一条能走到 Running 的路径**上写。`calculateStatus`
  是按 `box.Spec.Paused` 做状态跳转的(`sandbox_controller.go:648`、`:666`),phase 跳转会绕过只在
  创建时刷新的逻辑。
- **收回可寻址要显式写。** 要让一个 Sandbox 不可寻址,写 `Hostname` + 空 `Address`,**不能写 nil**。
  nil 的语义是 `Direct`,那会把所有消费方重新指向 Pod IP,而 `Hostname` 下那个 IP 是占位值。status 是
  用 `MergePatchType` 打的(`sandbox_controller.go:516`)、`Endpoint` 是 `omitempty`,所以省略字段留在
  etcd 里的是旧值而不是清空;真要彻底删掉得显式写 JSON null,而整体 marshal status 表达不出来。

凭据的 key 沿用 `runtimecredentials` 那套:`token` 放 bearer 或不透明值,front 要 mTLS 时用
`client.crt` / `client.key` / `ca.crt`。各消费方通过 informer cache 读 Secret(顺带解决轮转),
把解析出来的字符串传给 resolver,所以那个叶子包仍然不碰 Kubernetes 类型。gateway 数据面由 Go
filter 解析并注入 header,因为 Envoy 没法逐请求读 Secret。`Route.AccessToken` 本来就走同一条路,
并且有不许记日志的规则(`pkg/sandboxroute/AGENTS.md`)。

这个引用刻意不带 namespace,免得 controller 之外的东西把控制面组件指向任意一个 Secret;具体在哪个
namespace 下解析见[待定问题](#待定问题)。而且既然没有意图 API,也就不存在"照抄租户填的值"这回事 ——
整个 endpoint 都是 controller 自己推导出来的。

### 解析

每个访问点都按这个 Sandbox 自己的字段,逐请求问同一个 resolver。endpoint 为空、mode 为空或显式
`Direct` 都返回 `podIP:port`,请求本身不动。`Hostname` 连的是 `Address`,而它的端口是固定的,
**所以目标端口没法靠连接表达出来**,只能写在请求里 —— 通过 `Authority`、`PathPrefix`、`Headers`,
或者它们的任意组合。其他 mode 都是错误,不是 `Direct` 的兼容别名:

```
authority    GET https://3000-sbx7f3a.sbx.example.com/init
                 Host: 3000-sbx7f3a.sbx.example.com
                 endpoint.authority: {port}-sbx7f3a.sbx.example.com
path         GET https://sbx-alb.example.com/kruise/sbx7f3a/3000/init
                 front restores :path = /init
                 endpoint.pathPrefix: /kruise/sbx7f3a/{port}
header       GET https://sbx-alb.example.com/init
                 e2b-sandbox-id: sbx7f3a  e2b-sandbox-port: 3000
                 endpoint.headers: {e2b-sandbox-id: sbx7f3a,
                                    e2b-sandbox-port: "{port}"}

all three connect to endpoint.address = sbx-alb.example.com:443
```

这三种形状本仓库都已经在解析(`native_e2b.go:45` 和 `:28-30`、`customized_e2b.go:34-62`),所以让
`sandbox-gateway` 当 front 的话一行解析代码都不用加;而因为 header 名来自对象,换一个用别的 header
名的 front 也不需要改客户端。
三种形状都不要求 authority 参与 DNS 或证书校验:渲染后的 authority 只写到 Host / `:authority`,
连接 URL 仍然使用 front 自己的 `Address`,所以标准 Go transport 会从 `Address` 推导 TLS
`ServerName`。这些只针对**内部那一跳**,不要和部署里现成的面向客户端的通配 ingress
(`config/sandbox-manager/ingress.yaml:18`)混起来 —— 后者确实需要通配证书,因为浏览器要去解析那些
域名。

由于 `Address` 是按 Sandbox 来的,数据面不能再用一个静态 cluster 指向某一个 front。两个数据面现在
各带两个 `dynamic_forward_proxy` cluster(一个明文、一个 TLS),按请求由 Go filter 设的内部 header
选择,front 的 host 和端口通过 filter state 交给 DFP filter。另一条路 —— 要求"同一集群里所有
`Hostname` Sandbox 共用一个地址"、继续用静态的 `STRICT_DNS` —— 已经被否:这套方案面向的那些托管后端,
暴露出来的恰好就是 per-instance 的 endpoint。控制面不受影响,Go 客户端让它连哪儿就连哪儿。

path 前缀是三者里唯一会碰到协议层的,两处都刚好成立:`NewProcessClient` 是把 procedure 拼在 base URL
后面的,所以带前缀的 base URL 会得到 `/kruise/<id>/<port>/process.Process/Start`,front 改写完正好
还原;`Map` 用的是 `SplitN(path, "/", 3)`,所以 `/files` 请求的 query 不会丢。

### 路由投影和就绪判断

`sandboxroute.Route` 要带上这个 endpoint:两个数据面都得按请求从 Sandbox 的字段决定选哪个 cluster,
这个信息必须能通过投影传到 gateway registry 和 ext-proc store。resourceVersion 排序、身份替换、
删除水位都不受影响。
原来的 Pod IP 判断改成判断"能不能寻址"。纯值语义留在叶子 resolver 包里;API-aware 的共用投影是
`pkg/utils.IsSandboxAddressable`,因为 `pkg/cache` 已经被 `pkg/utils/runtime` 依赖,不能再反向 import
runtime:

```
addressable(sandbox) =
    Mode == Direct   && podInfo.podIP != ""
 || Mode == Hostname && endpoint.address != ""
```

`Direct` 的行为不变,上面那五处都改成调这个共用判断。**这是评审时最容易漏掉的改动**:那几处看上去
都只是一句无害的空值检查。

### resolver 放哪

新建一个不 import 任何仓内包的叶子包(`pkg/sandboxendpoint`),入参就是模式、host、端口这几个普通
值。两个看起来顺手的位置都不行:`pkg/sandboxroute` import 了 `pkg/identity`,而后者的依赖里已经有
`pkg/utils/runtime`,再反过来 import 就成环;`pkg/servers/e2b/adapters`(路由 header 常量现在在
那儿)会把 `pkg/servers/**` 拖进 `agent-sandbox-controller` 的依赖闭包。所以常量挪到叶子包里,
`adapters` 反过来引用。这两条 CI 都能验:`go list -deps ./cmd/agent-sandbox-controller/...` 里不能
出现 `pkg/servers/e2b/adapters`,并且编译不能有循环依赖。

### 各处怎么改

**A / A' / B。** `resolveBaseURL` / `dialIPFor` / `resolveTransport`(`client.go:283-325`)改成问
resolver,不再读 `Status.PodInfo.PodIP`。`Hostname` 下客户端就是个普通 `http.Client`,不用那套强制
指定 IP 的 transport,因此 runtime 的 TLS 材料在这次调用上不生效 —— 见
[TLS、信任边界和出错](#tls信任边界和出错)。`proxyutils.DefaultRequestFunc` 也改成走同一个 resolver,顺便去掉 CDP 那条路
自己拼 URL 的代码。

**C。** ext-proc 逐请求解析 route 上的 endpoint。`Direct` 仍然用 `x-envoy-original-dst-host`,只多一个
显式的 `direct` 路由标记。`Hostname` 下它设置 authority、给 path 加前缀、加上 endpoint 自带的 header,
并通过内部 header 把 front 的 host 和端口交给 DFP filter —— 全部用 `OVERWRITE_IF_EXISTS_OR_ADD`,
并且排在客户端自己的 `request-header-modifier` 之后,所以客户端换不掉它们 —— 最后清掉 route cache
让选中的 cluster 生效。

**D。** filter 侧同理,另外多一个 fail-closed 的开场动作:在解析之前先删掉内部 endpoint header 并写上
`direct` 标记,这样每一条提前 `Continue` 的路径都还是今天的行为。两个 `ORIGINAL_DST` cluster 都留给
`Direct`,包括第二个用 mTLS 重新加密到 runtime 的那个;`Hostname` 下这一段归 front,所以
`upstream-mtls` 分支直接跳过,请求走 front 的那两个 cluster。于是混用的集群里四个 cluster 同时在跑,
而 gateway 只在 `Direct` 时需要持有 runtime 的客户端证书。

**E,`ReturnPodIP`。** 客户端要 IP 是因为打算拿去连,占位值满足不了,所以这个扩展现在返回解析出来的
地址:`infra.Sandbox.GetIP()` 已改成 `GetEndpointAddress()`,`Direct` 下返回 Pod IP,`Hostname` 下返回
front 的 `host:port`,完全不可寻址时返回空。这样 metadata key 的含义不变,只是 `Hostname` 下它的值不再
是 IP;另外两个被否的选择是直接报错,或者声明这个扩展只支持 `Direct`。

**F,`status.sandboxIp`。** 不动,对 `Direct` 是准确的;`Hostname` 下它可能是个没人会读的占位值。

### TLS、信任边界和出错

L7 front 得读请求才能路由,所以它必须终止连接、再重新加密。`Hostname` 下调用方**完全不再应用 runtime
的 TLS 材料**,那一段归 front,而 `sandbox-gateway` 现成就能做(`enable-runtime-mtls`)。**这是让
加密链路中间多了个终止点,不是安全增强**:`Direct` 配支持 TLS 的 Sandbox 是从调用方一路到 runtime
的 mTLS,而 `Hostname` 多了一个能看到明文和 `X-Access-Token` 的环节。选它的理由只能是网络不通。

front 自己到 runtime 那一跳怎么加密,是 front 的配置 —— 和本提案交给部署方的其他 front 行为一样。
**这一跳之内**有两条值得写明,因为它们是本仓库自己的行为。

`TransportOptionsFor`(`transport.go:266`)对声明了 `AnnotationRuntimeTLSPort` 的 Sandbox,在调用方
没有证书时宁可报错也不降级成明文;而 `Hostname` 是在那个开关**之前**就解析 front 的
(`client.go:283`),所以这种 Sandbox 会以"不出示客户端证书"的方式被调用,**不管本地有没有配好证书**。
这是"那一段归 front"的自然结果,但它恰好是代码平时唯一拒绝的那种降级,所以每次尝试的日志改成报
`transport=front` 和 `runtimeTLSApplied=false`,而不是 TLS 模式那套值(那套值会写出一个实际从没连过
的 dialTarget)。

另外请求走的是普通明文 client,所以 `scheme: https` 时校验的是 front 的证书、用进程的系统信任库,
也不会出示客户端证书。在 `AuthSecretRef` 落地之前,`Hostname` 因此要求 front 自己不需要凭据、且证书
公共可信;而 `scheme: http` 下 access token 在这一跳是明文 —— ACK 那次验证就是这么跑的。

调用方渲染哪个端口也是我们这边决定的(开了 TLS 用 `tlsPort`,否则 `RuntimePort`),但 `Hostname` 下它
不再决定调用方自己的传输,只决定告诉 front 去连哪个端口。

`TransportOptionsFor` 对 `Direct` 的行为一条不变。

控制面调用本来就带着这个 Sandbox 的 `X-Access-Token`,而 gateway 存在 route 上的就是同一个值,
所以 `enable-auth` 开着、JWT 校验关着的时候能原样通过(`filter.go:168-177`)。开了 traffic JWT
校验的 route 是例外:gateway 要求一个绑定了 Sandbox ID 和 UID 的 JWT(`filter.go:157-163`),而
manager 不会带。倾向的解法是在 front 上单独开一个控制面入口,只校验 `X-Access-Token`、不要
traffic JWT;在它做出来之前,`Hostname` 不支持开了 `RequireTrafficAuth` 的 Sandbox。

runtime client 把 4xx 当永久失败、5xx 当临时失败(`APIError.IsClientError`),所以 front 对暂时
路由不到的 Sandbox 必须返回 5xx 而不是 404,否则启动期间的重试会停掉。本地的两类失败也照这个分:
传输配置错误(比如证书 bundle 不可用)是永久错误,第一次尝试就报出来;而"endpoint 暂时不可寻址"
是可重试的 —— 那是 writer 还没发布地址时的正常状态。

分层上:resolver 不含 API 模型和业务规则,拿到的是 endpoint 的各个字段加解析好的凭据,都是普通值;
没有包级全局开关,也不用 feature gate,`pkg/features` 只给 controller 用。

## 配置

任何组件都不加新 flag,也没有任何按 Sandbox 的配置项:字段由 writer 推导,消费方从对象上读。只有
Envoy 配置要改 —— 多一个 DNS cache、一个 `dynamic_forward_proxy` filter 和 `sandbox_front_http` /
`sandbox_front_https` 两个 cluster,route 分支按内部路由 header 匹配;manager 侧在
`config/sandbox-manager/envoy-config.yaml`,gateway 侧在 `config/sandbox-gateway/configmap.yaml`
加上 runtime-mTLS 的 overlay patch。`Direct` 流量仍走原来的 `ORIGINAL_DST` cluster,所以从不写
endpoint 的部署行为和今天完全一样。spike 还把 manager 的 Envoy 镜像固定到 `v1.37.3`(原来是
`v1.33-latest`);部署方要确认自己的 Envoy 版本支持这里用到的 DFP 和 `set_filter_state` 配置。

`endpoint.address` 和 `--e2b-domain` 不是一回事,后者是 API 响应里返回给客户端的域名。

## 对 L7 front 的要求

- 能按模板用到的写法,把请求路由到对应的 Sandbox **和端口**。front 如果不能从请求里得到上游端口,
  就得给每个 `(Sandbox, port)` 建一条规则。
- 能只按 `Host` / `:authority` 路由,不要求它和 TLS SNI 一致。
- 用 path 写法时,转发前去掉前缀、还原原始 path。
- 选择 header 写法时,front 必须支持按任意 header 值路由;ingress-nginx 没有这种通用原生能力。
- 分别保持两跳所需的协议。调用方到 front 和 front 到 runtime 是两跳:这次验证的公共 envd 明文
  49983 只支持 connect-over-HTTP/1.1,不支持 h2c gRPC;要走 gRPC,部署必须用 TLS/ALPN,或明确支持
  h2c 的 runtime 构建。
- 不缓冲 body,这样 `/files` 上传和流式 RPC 才能走。
- idle timeout 大于最长的流 —— 上游 E2B 设的是 610s,为了超过 GCP LB 的 600s。
- 对暂时路由不到的 Sandbox 返回 5xx,不要 404。ingress-nginx 默认返回 404,部署时必须显式改。

## 兼容性和回滚

- `status.endpoint` 为空就是 `Direct`:不用迁移,也没有现有字段改变含义。
- 只是新增一个可选 status 字段,所以要跑 `make generate manifests`,并且 CRD 要先于开始写它的
  controller 上线。上线顺序:CRD、各消费方、最后 controller。
- 不认识这个字段的进程会把所有 Sandbox 当 `Direct` —— 对 `Direct` 是对的,对 `Hostname` 会明确
  出错。
- 在有东西开始写这个字段之前,所有部署的行为完全不变:字段为空,所有 Sandbox 解析成 `Direct`。
- 回滚不对称:字段写在对象上,回滚进程并不会把寻址方式退回去,得把 `status.endpoint` 清掉,而且要
  显式 patch 成 null —— 没有别的东西会去清 writer 写进去的值。

## 风险

- **没升级的组件会去连占位 IP。** 共用的判断和 resolver 是把 Sandbox 变成目标地址的唯一入口,
  上线顺序也把消费方排在前面。这是把判断挪到对象上带来的主要风险。
- **路由 header 是客户端可以随便填的。** 只按它路由的 front 不能对不可信客户端开放,除非它自己
  校验 token。CDP 端口连 token 都没有。所以两个数据面都把内部 endpoint header 当作纯控制面输出:
  gateway filter 在解析前先删掉它们,ext-proc 在客户端的 header modifier 之后覆盖写入。
- **这个字段暂时没人写。** 一个不守上面那份契约的 writer 在本仓库的测试之外,所以消费方
  能自己兜住的就只有:每次解析都 `Validate`、未知 mode fail closed、以及 `Hostname` + 空 address 解析
  成"不可寻址"而不是退回 Pod IP。
- **过期的 endpoint 会被静默沿用。** merge patch 的省略语义和 phase 跳转都可能让上一代的 endpoint
  留在对象上,而它和过期的 Pod IP 不一样 —— 不会简单地连不上,而可能连到一个活着的 front、再落到
  错误的 Sandbox 上。缓解手段就是上面那几条写入规则;兜底是 runtime 那层的
  per-Sandbox `X-Access-Token`。
- **读 Secret 的权限范围。** 按 Sandbox 配凭据会让三个组件多出原本不需要的 Secret 读权限,而一个
  租户能影响的引用就是典型的 confused deputy —— 所以 namespace 固定。去掉意图 API 是把这一类问题
  消掉而不是缓解:controller 之外没有任何东西参与这个字段。凭据不得进日志,现有的 route 规则就是
  这么要求的。
- **两跳的协议能力不一样。** ACK 验证确认,公共 envd 的明文 49983 不支持 h2c,所以
  `connect.WithGRPC()` 即使直连也会失败。另外 `newPinnedTransport` 没设 `ForceAttemptHTTP2`,而自
  定义了 `DialContext` 又给了 `TLSClientConfig` 时 Go 不会自动开 HTTP/2。部署必须分别验证
  client-to-front 和 front-to-runtime 的实际协议组合,不能假设"端到端 HTTP/2"。

## 测试计划

resolver 的表驱动单测(两种模式、三种写法单独和组合、每条校验规则)和 `addressable` 的表驱动单测
(重点是带占位 Pod IP 的 `Hostname` Sandbox,要能只凭 host 判定可达)。三种写法各做一次经
`adapters.E2BAdapter.Map` 的往返。一致性测试断言控制面、ext-proc 和 gateway filter 对同一输入产出
一样的 authority、path 和 header。依赖断言保证 `pkg/servers/e2b/adapters` 不进 controller 闭包。
e2e 用**两类混着放**的 SandboxSet —— 只测一种模式的话,就算按 Sandbox 判断完全没生效也能过。

因为本仓库没有东西会写这个字段,单测和这个 e2e 都是通过 status 子资源直接写
`status.endpoint`。也就是说它们覆盖的是消费方和共用的就绪规则;writer 契约在 writer 所在的地方验证。

### 已验证的 spike 证据

部分实现已经发布在
[`BSWANG/agents:spike/endpoint-addressing-ack-validated`](https://github.com/BSWANG/agents/tree/spike/endpoint-addressing-ack-validated)。
在一次 ACK 部署中,同一个 manager 进程对混合 SandboxSet 按对象解析出了两条不同路径:

```text
Direct:   http://172.26.98.95:49983/json/version
Hostname: http://10.16.2.10:80/json/version
```

ingress-nginx 根据 Hostname Sandbox 的 authority 规则选中目标,转发到 `172.26.98.96:49983`。再把
这个 Sandbox 上报的 Pod IP 改成占位值 `169.254.1.1` 后,route 仍为 `running`,请求仍连接 front。
最终 `/json/version` 返回 404,是因为 envd 的 49983 listener 没有 CDP 路径;这次验证证明的是 endpoint
选择和路由,不是这个端口上的 CDP 功能。

那次验证时 endpoint 是由一个基于注解的 writer 写进去的,本次修订把它删掉了:分支上不再有意图注解,
同样的场景改成直接 patch status 子资源上的 `status.endpoint` 来复现。上面解析出来的那两条路径,和字段
从哪来没有任何关系。

## 备选方案

**进程级配一种模式、端点靠算** —— 每个进程一种模式加一套模板,端点由 `(sandboxID, port)` 算出来。
更省事,不用改 API、重启就能回滚,但表达不了两类混存,也没法把占位 Pod IP 和真实地址区分开。

**用注解或 spec 字段承载意图** —— 在 SandboxSet 上写一份模板、传到每个 Sandbox 上、由 controller
渲染。spike 最早就是这么验证的,后来被否掉:决定性的输入是后端,而 controller 本来就知道;所以这个
开关只是为"没人会按 Sandbox 去选的事"多加了一层 API、一套模板语言和一组占位符 —— 而且会出现在每个
最终用户的对象上。

**把路由 header 放到对象上** —— header 的值就是对象上已有的 Sandbox ID,名字属于 front。

**endpoint 放在 `status.podInfo` 里** —— 那不是 Pod 的属性,而且 `podInfo` 可能是空的。

**扩展 `AnnotationRuntimeURL`** —— TLS 模式下会被忽略,覆盖不到 CDP 和数据面,表达不了端口,
而且现在没人写它。

其他项目的做法都不适用:E2B 是让每个节点的 orchestrator 在同机上直连 envd(搬过来就是一个
DaemonSet 加一份节点映射表,而且 Pod 网络从集群外不通时照样解决不了);
`kubernetes-sigs/agent-sandbox` 是 router 转发到 headless Service 域名(客户端侧和本提案同形,
但仍需要能直连 Pod 的组件);Coder 的反向隧道对网络要求最低,但要给 `agent-runtime` 加出向连接
管理;`pods/proxy` 不用加组件,但 HTTP/2 流式支持弱。

## 实施计划和状态

1. **已实现:**`status.endpoint` API、模式类型、deepcopy 和 CRD。
2. **已实现:**叶子包 `pkg/sandboxendpoint` 的解析和校验 —— mode、address、scheme、path 前缀、
   authority、header 名、未知占位符、`{port}` 是否存在 —— 以及 addressable 纯值语义和
   `pkg/utils.IsSandboxAddressable` 的 API-aware 投影。消费方对未知 mode 报错,不会当 `Direct`。
   路由 header 常量下沉到叶子包还没做。
3. **已实现:**`sandboxroute.Route` 投影并深拷贝 endpoint、逐请求解析;Pod IP 判断和其余四处就绪
   判断都换成共用的 addressable 规则。
4. **已实现:**`pkg/utils/runtime` 和 `pkg/utils/proxyutils` 按 Sandbox 解析;runtime 和 CDP
   路径已经在真实 ACK 混合部署里走过。
5. **部分实现:**endpoint accessor `GetEndpointAddress()` 和基于它的 `ReturnPodIP`。informer
   Secret 解析、RBAC 和 `AuthSecretRef` 未实现;在它们落地之前,`Hostname` 的前置条件是 front 自己
   不需要凭据,且在 `scheme: https` 下证书是公共可信的。
6. **刻意不做:**writer 和意图 API 都不在本提案范围内。[设计](#字段)一节里的 writer 契约对最终写这个
   字段的东西都是规范性的;测试直接写 `status.endpoint`。
7. **已实现:**manager ext-proc 和 gateway filter 的请求改写、DFP cluster 选择,以及两个数据面的
   Envoy 配置。有单测覆盖,但都还没在真实流量下跑过。
8. **部分实现:**单测和控制面的 ACK 混合部署验证。三个消费方的一致性测试、三种 slot 的 adapter
   往返、部署文档和长期 e2e job 还没完成。

以上全部都在验证分支上。它证明的是控制面 resolver、共用就绪契约,以及两个数据面的形状 —— 不包括
凭据设计,也不包括两个数据面在真实流量下的表现。

## 已定的事

- **意图不做成 API。** 不加注解、不加 spec 字段:endpoint 由写它的那方从后端推导,而字段、resolver
  和消费方对所有部署都是同一套(2026-08-20)。
- **writer 这一版不做。** 字段一直为空,直到 controller 扩展或部署方自己的 webhook 去写它。
- **支持 per-Sandbox 地址。** 两个数据面都带 `dynamic_forward_proxy` cluster,不要求全集群共用一个
  front 地址。
- **manager 侧的 Envoy 数据面在范围内**,和 gateway 一起实现,不推后。

## 待定问题

1. **认证 Secret 放哪个 namespace** —— Sandbox 自己的(读权限会摊到所有 sandbox namespace),
   还是系统的(RBAC 收得住,但凭据就成了运维侧的东西而不是租户的)?
2. **front 的认证**:单独开一个控制面入口,还是要求控制面调用像其他客户端一样带 traffic JWT?
3. **runtime 调用该带哪个端口,以及那个端口上是谁。** ACK 验证确认 `utils.RuntimePort`(49983)
   上是 envd —— 已发布的 `agent-runtime` 镜像构建的就是上游 envd 并把它拷进沙箱容器 —— 且它的
   明文 listener 只说 connect-over-HTTP/1.1,不支持 h2c gRPC。`RuntimeTLSPort`(49984)是另一个
   由 controller 通过 `AnnotationRuntimeTLSPort` 广告出来的 listener,而本仓库里并没有服务它的
   代码,所以换端口可能不只是换地址,而是换成另一个服务、另一套协议。同时 gateway 是在
   `sandboxPort == utils.RuntimePort` 时走 runtime-mTLS 分支,也就是打向明文的 envd listener ——
   这一点建议让 gateway owner 确认是否有意。endpoint 能表达所有端口,但哪个端口提供哪套 API
   是部署前置条件,不是寻址细节。另外两个面对"runtime 是哪个端口"的答案本来就不一致:调用方开了
   TLS 渲染 49984,而 gateway 的 mTLS 分支按 49983 判断。
4. **占位 Pod IP**:`Hostname` 下 writer 还继续写一个,还是把 `sandboxIp` 和 `podInfo.podIP` 留空?
   消费方已经不关心了,但 `kubectl` 的输出和运维会关心。

## 实施历史

- [x] 2026-08-13: 提案起草。
- [x] 2026-08-17: 按"两类混存 + 占位 IP"的需求,改成围绕 per-Sandbox endpoint 字段设计。
- [x] 2026-08-18: 在
      [`spike/endpoint-addressing-ack-validated`](https://github.com/BSWANG/agents/tree/spike/endpoint-addressing-ack-validated)
      发布 API、resolver、runtime/CDP 集成、共用 addressable 和 route 投影。
- [x] 2026-08-18: 部署到 ACK,通过真实 manager 和 ingress-nginx 验证 Direct/Hostname 混用及占位
      Pod IP 行为。
- [x] 2026-08-18: 完成两个数据面 —— ext-proc 和 gateway 的请求改写、DFP cluster 选择、Envoy 配置 ——
      以及 `ReturnPodIP` 背后的 endpoint accessor。
- [x] 2026-08-20: 定了 writer 这件事:字段、resolver 和消费方对所有部署完全一致;endpoint 由知道后端
      寻址规则的那方推导;不加意图注解也不加 spec 字段;writer 本身推后,所以 spike 分支上基于注解的
      writer 已删除。
- [ ] 凭据(`AuthSecretRef`)、三个消费方的一致性测试、数据面在真实流量下的验证,以及长期 e2e。
