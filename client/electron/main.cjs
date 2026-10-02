// Electron 主进程——桌面壳的全部逻辑（刻意保持最小）。
// 职责：创建一个窗口，开发时加载 Vite 开发服务器，打包后加载构建好的页面。
// 业务界面还是那套 Vue 代码，一行没改——"同一份前端，两种出口"。
const { app, BrowserWindow, Menu } = require('electron')
const path = require('path')

function createWindow() {
  const win = new BrowserWindow({
    // v0.30：界面按 1600×900（16:9）设计稿等比缩放，窗口默认也开成 16:9；
    // useContentSize 让宽高指"网页内容区"而非含标题栏的外框，内容区正好 16:9。
    width: 1600,
    height: 900,
    useContentSize: true,
    title: '黄金零售系统',
    webPreferences: {
      contextIsolation: true, // 安全默认：渲染页面与Node隔离
    },
  })

  const devUrl = process.env.VITE_DEV_SERVER_URL
  if (devUrl) {
    win.loadURL(devUrl) // 开发模式：连 Vite，热更新照常
  } else {
    win.loadFile(path.join(__dirname, '../dist/index.html')) // 打包模式
  }
}

app.whenReady().then(() => {
  // 精简菜单（保留编辑快捷键，Cmd+C/V 才能用）
  Menu.setApplicationMenu(Menu.buildFromTemplate([
    { label: '黄金零售系统', submenu: [{ role: 'about', label: '关于' }, { type: 'separator' }, { role: 'quit', label: '退出' }] },
    { label: '编辑', submenu: [{ role: 'undo' }, { role: 'redo' }, { type: 'separator' }, { role: 'cut' }, { role: 'copy' }, { role: 'paste' }, { role: 'selectAll' }] },
    { label: '视图', submenu: [{ role: 'reload', label: '刷新' }, { role: 'toggleDevTools', label: '开发者工具' }, { type: 'separator' }, { role: 'resetZoom' }, { role: 'zoomIn' }, { role: 'zoomOut' }] },
  ]))
  createWindow()
  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow()
  })
})

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit()
})
