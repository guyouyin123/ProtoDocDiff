package main

var (
	//项目下proto存放的路径
	apiPath = "/api"

	//环境列表
	envs = []string{"localhost", "woda-alpha", "woda-sit"}

	// 项目根目录
	rootDirMap = map[string]string{
		"企微": "/Users/jeff/Desktop/service",
	}

	// 文档输出目录
	docDir = "/Users/jeff/myself/doc/ProtoDocDiff_index"

	// Consul转发 地址
	consulAddr = "http://192.168.150.9:8080/ConsulRun"

	// 近一月活跃分支数量
	MaxBranches = 5
)
