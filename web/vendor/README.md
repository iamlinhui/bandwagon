# 前端第三方资源(已内嵌进二进制)

这些文件不是本项目代码,按原样内嵌(`go:embed`),运行时**不访问任何 CDN**。

| 文件 | 来源包 | 版本 | 许可证 |
| --- | --- | --- | --- |
| `xterm.js` / `xterm.css` | npm `@xterm/xterm` | 5.5.0 | MIT |
| `xterm-addon-fit.js` | npm `@xterm/addon-fit` | 0.10.0 | MIT |
| `xterm-addon-web-links.js` | npm `@xterm/addon-web-links` | 0.11.0 | MIT |

均为 UMD 构建,加载后挂到全局:

- `window.Terminal`
- `window.FitAddon.FitAddon`
- `window.WebLinksAddon.WebLinksAddon`

下载地址形如 `https://cdn.jsdelivr.net/npm/@xterm/xterm@5.5.0/lib/xterm.js`。

**更新方法**:下载新版本覆盖同名文件即可(注意 addon 与 xterm 主包的版本兼容)。
