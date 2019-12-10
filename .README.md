# pforward

## Name

*forward* - facilitates proxying DNS messages to upstream resolvers and collect all IP addresses from upstream.

## Description

Based on CoreDNS built-in plugin [forward](https://github.com/coredns/coredns/tree/2503df905638710a171f61900b59d1e64316a306/plugin/forward).

## 说明

基于 CoreDNS 内建插件 [forward](https://github.com/coredns/coredns/tree/2503df905638710a171f61900b59d1e64316a306/plugin/forward)。

用法请参照官方文档。

## 用法

替换官方插件：

plugin.cfg

```
forward:github.com/microdog/pforward
#forward:forward
```

## 行为

本插件会同时请求所有 upstream resolvers，并尝试收集并返回所有 A 和 AAAA 记录。

## 告

就是个随便练手写着玩的项目，不保证任何东西。主要是为了配合 [boost](https://github.com/microdog/boost) 插件使用。
