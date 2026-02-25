[toc]

# 文档生成器说明

## 项目简介（ProtoDocDiff）

ProtoDocDiff 是一个面向 gRPC/Proto 项目的文档导航与调试工具。它从 proto 与 Go 代码中自动提取接口定义、请求/响应结构与字段说明，生成可浏览、可检索的 API 文档；同时支持分支对比，在页面以 NEW/MODIFIED 标签直观标注变更。
注意：每次重复执行，会自动检测分支的最近提交时间是否变化。只有分支的提交时间发生变化，才会重新生成文档。
## 亮点特性

- 分支差异标注
  - 与 `master`/`main` 对比，自动识别“新增/修改”的接口与字段
  - 在侧边目录与详情页标题显示红色徽标：`NEW`/`MODIFIED`
  - 在参数表格中对具体字段追加红色标签 `[NEW]`、`[MODIFIED]`
- 自动文档生成
  - 解析 proto 获取服务/方法，联动解析 Go 结构体生成请求/响应示例与字段表
  - 兼容多种请求体识别模式（`json.Unmarshal` 目标变量、`new(Type)`/`&Type{}`/具名变量等）
  - 支持从封装函数中推断响应类型（如 `NewCommonRet`/`GenerateJSON`/`MakeBaseRsp`）
- 高效导航与布局
  - 三栏布局：左侧目录、居中内容、右侧开发统计与“API 变更”列表
  - 目录分组可折叠、默认折叠并记忆展开状态；当前接口高亮
- 在线调试能力
  - 统一调试弹窗，历史记录最多 10 条，进入时默认选择最近一条并回填
  - 返回结果以树形 JSON 查看（折叠/展开，高亮显示）
- 增量与容错
  - 仅在分支变更时增量生成；失败/非 git 目录自动降级
  - 读取 `branches.json` 维护有效分支，自动清理陈旧产物


## 作用与输出

- 自动扫描已配置的服务项目与分支，解析 `proto` 与相关 `Go` 结构，生成可浏览的 API 文档页面。
- 输出位置由配置项控制：`doc/conf.go` 的 `docDir`。页面包含：左侧目录、居中 API 内容、右侧统计（接口/开发者），并集成请求调试弹窗（可记录历史）。

## 工作流程（概要）

- 读取 `rootDirMap` 下的项目，过滤出包含 `api/*.proto` 的服务项目。
- 获取远端活跃分支，使用工作树检查出分支代码，解析 `proto` 以获取服务与方法；解析 `Go` 结构用于示例请求/响应与字段表格。
- 生成每个项目分支的文档，并更新分组导航与根导航。
- 调试弹窗通过 `consulAddr` 发送请求，响应以 `{code,message,data}` 展示。

## 配置项（位于 `doc/conf.go`）

- `rootDirMap`：服务项目根目录映射（分组名 → 路径）。
- `docDir`：文档输出根目录。
- `consulAddr`：调试请求转发地址。
- `MaxBranches`：每个项目生成的活跃分支上限。

## proto 编写规范

**兼容了我们的祖传代码，无需特意遵守什么规范**

### 字段注释（请求体）

- 每个字段必须给出用途说明；如为必填，在注释中包含「必填/必选/必须」任一关键词（生成器会据此标注）。
- 枚举值需在注释中列出取值及含义；数组需说明元素含义与上限。
- 示例：`// 用户唯一标识，必填`、`// 开始时间，格式：YYYY-MM-DD HH:mm:ss`。

### 字段注释（响应体）

- 为每个字段提供清晰的业务含义，必要时说明单位、格式与取值范围。
- 嵌套对象需在该对象的 `message` 定义处补充字段注释，便于生成器展开字段表格。

### 分类与显示名（用于目录分组与中文展示）

- 在方法的注释中建议按「第一行：分类名」「第二行：方法中文名」组织，生成器将按分类分组并在目录与卡片标题中使用中文名。
- 无分类时归入「未分类」。
- 每个分类下的Method数量必须>1,否则分类名视为接口名

## 规范示例1（proto）

```proto
service Woda_BrokerServiceList {
  //服务单
  rpc ServiceListCreate(AASMessage.InvokeServiceRequest) returns (AASMessage.InvokeServiceResponse); //创建服务单
  rpc ServiceListWebStatusEdit(AASMessage.InvokeServiceRequest) returns (AASMessage.InvokeServiceResponse); //web修改服务单状态

  //安置单
  rpc ServiceListWorkCallbackQuery(AASMessage.InvokeServiceRequest) returns (AASMessage.InvokeServiceResponse); //安置列表
  rpc ServiceListWorkCallbackEdit(AASMessage.InvokeServiceRequest) returns (AASMessage.InvokeServiceResponse); //安置修改状态
  rpc ServiceListWorkCallbackStatistics(AASMessage.InvokeServiceRequest) returns (AASMessage.InvokeServiceResponse); //安置统计
}
```

## 规范示例2（proto）

这样格式只有接口名，没有分类

```proto
service Woda_BrokerServiceList {
  //创建服务单
  rpc ServiceListCreate(AASMessage.InvokeServiceRequest) returns (AASMessage.InvokeServiceResponse); 
  //web修改服务单状态
  rpc ServiceListWebStatusEdit(AASMessage.InvokeServiceRequest) returns (AASMessage.InvokeServiceResponse); 
}
```

## 注意事项

- 生成器仅渲染文档，不修改业务逻辑；请求调试需要正确的 `consulAddr` 与环境配置。
- 若字段注释包含「必填/必选/必须」关键字，生成页面会将该字段标记为必填；否则视为可选。
