package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"imuslab.com/zoraxy/mod/auth"
	"imuslab.com/zoraxy/mod/dynamicproxy"
	"imuslab.com/zoraxy/mod/dynamicproxy/permissionpolicy"
	"imuslab.com/zoraxy/mod/uptime"
	"imuslab.com/zoraxy/mod/utils"
)

var (
	dynamicProxyRouter *dynamicproxy.Router
)

// Add user customizable reverse proxy
func ReverseProxtInit() {
	/*
		Load Reverse Proxy Global Settings
	*/
	inboundPort := 80
	if sysdb.KeyExists("settings", "inbound") {
		sysdb.Read("settings", "inbound", &inboundPort)
		SystemWideLogger.Println("Serving inbound port ", inboundPort)
	} else {
		SystemWideLogger.Println("Inbound port not set. Using default (80)")
	}

	useTls := false
	sysdb.Read("settings", "usetls", &useTls)
	if useTls {
		SystemWideLogger.Println("TLS mode enabled. Serving proxxy request with TLS")
	} else {
		SystemWideLogger.Println("TLS mode disabled. Serving proxy request with plain http")
	}

	forceLatestTLSVersion := false
	sysdb.Read("settings", "forceLatestTLS", &forceLatestTLSVersion)
	if forceLatestTLSVersion {
		SystemWideLogger.Println("Force latest TLS mode enabled. Minimum TLS LS version is set to v1.2")
	} else {
		SystemWideLogger.Println("Force latest TLS mode disabled. Minimum TLS version is set to v1.0")
	}

	developmentMode := false
	sysdb.Read("settings", "devMode", &developmentMode)
	if useTls {
		SystemWideLogger.Println("Development mode enabled. Using no-store Cache Control policy")
	} else {
		SystemWideLogger.Println("Development mode disabled. Proxying with default Cache Control policy")
	}

	listenOnPort80 := false
	sysdb.Read("settings", "listenP80", &listenOnPort80)
	if listenOnPort80 {
		SystemWideLogger.Println("Port 80 listener enabled")
	} else {
		SystemWideLogger.Println("Port 80 listener disabled")
	}

	forceHttpsRedirect := false
	sysdb.Read("settings", "redirect", &forceHttpsRedirect)
	if forceHttpsRedirect {
		SystemWideLogger.Println("Force HTTPS mode enabled")
		//Port 80 listener must be enabled to perform http -> https redirect
		listenOnPort80 = true
	} else {
		SystemWideLogger.Println("Force HTTPS mode disabled")
	}

	/*
		Create a new proxy object
		The DynamicProxy is the parent of all reverse proxy handlers,
		use for managemening and provide functions to access proxy handlers
	*/

	dprouter, err := dynamicproxy.NewDynamicProxy(dynamicproxy.RouterOption{
		HostUUID:           nodeUUID,
		HostVersion:        version,
		Port:               inboundPort,
		UseTls:             useTls,
		ForceTLSLatest:     forceLatestTLSVersion,
		NoCache:            developmentMode,
		ListenOnPort80:     listenOnPort80,
		ForceHttpsRedirect: forceHttpsRedirect,
		TlsManager:         tlsCertManager,
		RedirectRuleTable:  redirectTable,
		GeodbStore:         geodbStore,
		StatisticCollector: statisticCollector,
		WebDirectory:       *staticWebServerRoot,
		AccessController:   accessController,
	})
	if err != nil {
		SystemWideLogger.PrintAndLog("Proxy", "Unable to create dynamic proxy router", err)
		return
	}

	dynamicProxyRouter = dprouter

	/*

		Load all conf from files

	*/
	confs, _ := filepath.Glob("./conf/proxy/*.config")
	for _, conf := range confs {
		err := LoadReverseProxyConfig(conf)
		if err != nil {
			SystemWideLogger.PrintAndLog("Proxy", "Failed to load config file: "+filepath.Base(conf), err)
			return
		}
	}

	if dynamicProxyRouter.Root == nil {
		//Root config not set (new deployment?), use internal static web server as root
		defaultRootRouter, err := GetDefaultRootConfig()
		if err != nil {
			SystemWideLogger.PrintAndLog("Proxy", "Failed to generate default root routing", err)
			return
		}
		dynamicProxyRouter.SetProxyRouteAsRoot(defaultRootRouter)
	}

	//Start Service
	//Not sure why but delay must be added if you have another
	//reverse proxy server in front of this service
	time.Sleep(300 * time.Millisecond)
	dynamicProxyRouter.StartProxyService()
	SystemWideLogger.Println("Dynamic Reverse Proxy service started")

	//Add all proxy services to uptime monitor
	//Create a uptime monitor service
	go func() {
		//This must be done in go routine to prevent blocking on system startup
		uptimeMonitor, _ = uptime.NewUptimeMonitor(&uptime.Config{
			Targets:         GetUptimeTargetsFromReverseProxyRules(dynamicProxyRouter),
			Interval:        300, //5 minutes
			MaxRecordsStore: 288, //1 day
		})

		//Pass the pointer of this uptime monitor into the load balancer
		loadbalancer.Options.UptimeMonitor = uptimeMonitor

		SystemWideLogger.Println("Uptime Monitor background service started")
	}()
}

func ReverseProxyHandleOnOff(w http.ResponseWriter, r *http.Request) {
	enable, _ := utils.PostPara(r, "enable") //Support root, vdir and subd
	if enable == "true" {
		err := dynamicProxyRouter.StartProxyService()
		if err != nil {
			utils.SendErrorResponse(w, err.Error())
			return
		}
	} else {
		//Check if it is loopback
		if dynamicProxyRouter.IsProxiedSubdomain(r) {
			//Loopback routing. Turning it off will make the user lost control
			//of the whole system. Do not allow shutdown
			utils.SendErrorResponse(w, "Unable to shutdown in loopback rp mode. Remove proxy rules for management interface and retry.")
			return
		}

		err := dynamicProxyRouter.StopProxyService()
		if err != nil {
			utils.SendErrorResponse(w, err.Error())
			return
		}
	}

	utils.SendOK(w)
}

func ReverseProxyHandleAddEndpoint(w http.ResponseWriter, r *http.Request) {
	eptype, err := utils.PostPara(r, "type") //Support root and host
	if err != nil {
		utils.SendErrorResponse(w, "type not defined")
		return
	}

	endpoint, err := utils.PostPara(r, "ep")
	if err != nil {
		utils.SendErrorResponse(w, "endpoint not defined")
		return
	}

	tls, _ := utils.PostPara(r, "tls")
	if tls == "" {
		tls = "false"
	}

	useTLS := (tls == "true")

	//Bypass global TLS value / allow direct access from port 80?
	bypassGlobalTLS, _ := utils.PostPara(r, "bypassGlobalTLS")
	if bypassGlobalTLS == "" {
		bypassGlobalTLS = "false"
	}

	useBypassGlobalTLS := bypassGlobalTLS == "true"

	//Enable TLS validation?
	stv, _ := utils.PostPara(r, "tlsval")
	if stv == "" {
		stv = "false"
	}

	skipTlsValidation := (stv == "true")

	//Get access rule ID
	accessRuleID, _ := utils.PostPara(r, "access")
	if accessRuleID == "" {
		accessRuleID = "default"
	}
	if !accessController.AccessRuleExists(accessRuleID) {
		utils.SendErrorResponse(w, "invalid access rule ID selected")
		return
	}

	// Require basic auth?
	rba, _ := utils.PostPara(r, "bauth")
	if rba == "" {
		rba = "false"
	}

	requireBasicAuth := (rba == "true")

	// Require Rate Limiting?
	requireRateLimit := false
	proxyRateLimit := 1000

	requireRateLimit, err = utils.PostBool(r, "rate")
	if err != nil {
		requireRateLimit = false
	}
	if requireRateLimit {
		proxyRateLimit, err = utils.PostInt(r, "ratenum")
		if err != nil {
			proxyRateLimit = 0
		}
		if err != nil {
			utils.SendErrorResponse(w, "invalid rate limit number")
			return
		}
		if proxyRateLimit <= 0 {
			utils.SendErrorResponse(w, "rate limit number must be greater than 0")
			return
		}
	}

	// Bypass WebSocket Origin Check
	strbpwsorg, _ := utils.PostPara(r, "bpwsorg")
	if strbpwsorg == "" {
		strbpwsorg = "false"
	}
	bypassWebsocketOriginCheck := (strbpwsorg == "true")

	//Prase the basic auth to correct structure
	cred, _ := utils.PostPara(r, "cred")
	basicAuthCredentials := []*dynamicproxy.BasicAuthCredentials{}
	if requireBasicAuth {
		preProcessCredentials := []*dynamicproxy.BasicAuthUnhashedCredentials{}
		err = json.Unmarshal([]byte(cred), &preProcessCredentials)
		if err != nil {
			utils.SendErrorResponse(w, "invalid user credentials")
			return
		}

		//Check if there are empty password credentials
		for _, credObj := range preProcessCredentials {
			if strings.TrimSpace(credObj.Password) == "" {
				utils.SendErrorResponse(w, credObj.Username+" has empty password")
				return
			}
		}

		//Convert and hash the passwords
		for _, credObj := range preProcessCredentials {
			basicAuthCredentials = append(basicAuthCredentials, &dynamicproxy.BasicAuthCredentials{
				Username:     credObj.Username,
				PasswordHash: auth.Hash(credObj.Password),
			})
		}
	}

	var proxyEndpointCreated *dynamicproxy.ProxyEndpoint
	if eptype == "host" {
		rootOrMatchingDomain, err := utils.PostPara(r, "rootname")
		if err != nil {
			utils.SendErrorResponse(w, "hostname not defined")
			return
		}
		rootOrMatchingDomain = strings.TrimSpace(rootOrMatchingDomain)

		//Check if it contains ",", if yes, split the remainings as alias
		aliasHostnames := []string{}
		if strings.Contains(rootOrMatchingDomain, ",") {
			matchingDomains := strings.Split(rootOrMatchingDomain, ",")
			if len(matchingDomains) > 1 {
				rootOrMatchingDomain = matchingDomains[0]
				for _, aliasHostname := range matchingDomains[1:] {
					//Filter out any space
					aliasHostnames = append(aliasHostnames, strings.TrimSpace(aliasHostname))
				}
			}
		}

		//Generate a proxy endpoint object
		thisProxyEndpoint := dynamicproxy.ProxyEndpoint{
			//I/O
			ProxyType:            dynamicproxy.ProxyType_Host,
			RootOrMatchingDomain: rootOrMatchingDomain,
			MatchingDomainAlias:  aliasHostnames,
			Domain:               endpoint,
			//TLS
			RequireTLS:               useTLS,
			BypassGlobalTLS:          useBypassGlobalTLS,
			SkipCertValidations:      skipTlsValidation,
			SkipWebSocketOriginCheck: bypassWebsocketOriginCheck,
			AccessFilterUUID:         accessRuleID,
			//VDir
			VirtualDirectories: []*dynamicproxy.VirtualDirectoryEndpoint{},
			//Custom headers
			UserDefinedHeaders: []*dynamicproxy.UserDefinedHeader{},
			//Auth
			RequireBasicAuth:        requireBasicAuth,
			BasicAuthCredentials:    basicAuthCredentials,
			BasicAuthExceptionRules: []*dynamicproxy.BasicAuthExceptionRule{},
			DefaultSiteOption:       0,
			DefaultSiteValue:        "",
			// Rate Limit
			RequireRateLimit: requireRateLimit,
			RateLimit:        int64(proxyRateLimit),
		}

		preparedEndpoint, err := dynamicProxyRouter.PrepareProxyRoute(&thisProxyEndpoint)
		if err != nil {
			utils.SendErrorResponse(w, "unable to prepare proxy route to target endpoint: "+err.Error())
			return
		}

		dynamicProxyRouter.AddProxyRouteToRuntime(preparedEndpoint)
		proxyEndpointCreated = &thisProxyEndpoint
	} else if eptype == "root" {
		//Get the default site options and target
		dsOptString, err := utils.PostPara(r, "defaultSiteOpt")
		if err != nil {
			utils.SendErrorResponse(w, "default site action not defined")
			return
		}

		var defaultSiteOption int = 1
		opt, err := strconv.Atoi(dsOptString)
		if err != nil {
			utils.SendErrorResponse(w, "invalid default site option")
			return
		}

		defaultSiteOption = opt

		dsVal, err := utils.PostPara(r, "defaultSiteVal")
		if err != nil && (defaultSiteOption == 1 || defaultSiteOption == 2) {
			//Reverse proxy or redirect, must require value to be set
			utils.SendErrorResponse(w, "target not defined")
			return
		}

		//Write the root options to file
		rootRoutingEndpoint := dynamicproxy.ProxyEndpoint{
			ProxyType:                dynamicproxy.ProxyType_Root,
			RootOrMatchingDomain:     "/",
			Domain:                   endpoint,
			RequireTLS:               useTLS,
			BypassGlobalTLS:          false,
			SkipCertValidations:      false,
			SkipWebSocketOriginCheck: true,

			DefaultSiteOption: defaultSiteOption,
			DefaultSiteValue:  dsVal,
		}
		preparedRootProxyRoute, err := dynamicProxyRouter.PrepareProxyRoute(&rootRoutingEndpoint)
		if err != nil {
			utils.SendErrorResponse(w, "unable to prepare root routing: "+err.Error())
			return
		}

		dynamicProxyRouter.SetProxyRouteAsRoot(preparedRootProxyRoute)
		proxyEndpointCreated = &rootRoutingEndpoint
	} else {
		//Invalid eptype
		utils.SendErrorResponse(w, "invalid endpoint type")
		return
	}

	//Save the config to file
	err = SaveReverseProxyConfig(proxyEndpointCreated)
	if err != nil {
		SystemWideLogger.PrintAndLog("Proxy", "Unable to save new proxy rule to file", err)
		return
	}

	//Update utm if exists
	UpdateUptimeMonitorTargets()

	utils.SendOK(w)
}

/*
ReverseProxyHandleEditEndpoint handles proxy endpoint edit
(host only, for root use Default Site page to edit)
This endpoint do not handle basic auth credential update.
The credential will be loaded from old config and reused
*/
func ReverseProxyHandleEditEndpoint(w http.ResponseWriter, r *http.Request) {
	rootNameOrMatchingDomain, err := utils.PostPara(r, "rootname")
	if err != nil {
		utils.SendErrorResponse(w, "Target proxy rule not defined")
		return
	}

	endpoint, err := utils.PostPara(r, "ep")
	if err != nil {
		utils.SendErrorResponse(w, "endpoint not defined")
		return
	}

	tls, _ := utils.PostPara(r, "tls")
	if tls == "" {
		tls = "false"
	}

	useTLS := (tls == "true")

	stv, _ := utils.PostPara(r, "tlsval")
	if stv == "" {
		stv = "false"
	}
	skipTlsValidation := (stv == "true")

	//Load bypass TLS option
	bpgtls, _ := utils.PostPara(r, "bpgtls")
	if bpgtls == "" {
		bpgtls = "false"
	}
	bypassGlobalTLS := (bpgtls == "true")

	// Basic Auth
	rba, _ := utils.PostPara(r, "bauth")
	if rba == "" {
		rba = "false"
	}

	requireBasicAuth := (rba == "true")

	// Rate Limiting?
	rl, _ := utils.PostPara(r, "rate")
	if rl == "" {
		rl = "false"
	}
	requireRateLimit := (rl == "true")
	rlnum, _ := utils.PostPara(r, "ratenum")
	if rlnum == "" {
		rlnum = "0"
	}
	proxyRateLimit, err := strconv.ParseInt(rlnum, 10, 64)
	if err != nil {
		utils.SendErrorResponse(w, "invalid rate limit number")
		return
	}
	if requireRateLimit && proxyRateLimit <= 0 {
		utils.SendErrorResponse(w, "rate limit number must be greater than 0")
		return
	} else if proxyRateLimit < 0 {
		proxyRateLimit = 1000
	}

	// Bypass WebSocket Origin Check
	strbpwsorg, _ := utils.PostPara(r, "bpwsorg")
	if strbpwsorg == "" {
		strbpwsorg = "false"
	}
	bypassWebsocketOriginCheck := (strbpwsorg == "true")

	//Load the previous basic auth credentials from current proxy rules
	targetProxyEntry, err := dynamicProxyRouter.LoadProxy(rootNameOrMatchingDomain)
	if err != nil {
		utils.SendErrorResponse(w, "Target proxy config not found or could not be loaded")
		return
	}

	//Generate a new proxyEndpoint from the new config
	newProxyEndpoint := dynamicproxy.CopyEndpoint(targetProxyEntry)
	newProxyEndpoint.Domain = endpoint
	newProxyEndpoint.RequireTLS = useTLS
	newProxyEndpoint.BypassGlobalTLS = bypassGlobalTLS
	newProxyEndpoint.SkipCertValidations = skipTlsValidation
	newProxyEndpoint.RequireBasicAuth = requireBasicAuth
	newProxyEndpoint.RequireRateLimit = requireRateLimit
	newProxyEndpoint.RateLimit = proxyRateLimit
	newProxyEndpoint.SkipWebSocketOriginCheck = bypassWebsocketOriginCheck

	//Prepare to replace the current routing rule
	readyRoutingRule, err := dynamicProxyRouter.PrepareProxyRoute(newProxyEndpoint)
	if err != nil {
		utils.SendErrorResponse(w, err.Error())
		return
	}
	targetProxyEntry.Remove()
	dynamicProxyRouter.AddProxyRouteToRuntime(readyRoutingRule)

	//Save it to file
	SaveReverseProxyConfig(newProxyEndpoint)

	//Update uptime monitor
	UpdateUptimeMonitorTargets()

	utils.SendOK(w)
}

func ReverseProxyHandleAlias(w http.ResponseWriter, r *http.Request) {
	rootNameOrMatchingDomain, err := utils.PostPara(r, "ep")
	if err != nil {
		utils.SendErrorResponse(w, "Invalid ep given")
		return
	}

	//No need to check for type as root (/) can be set to default route
	//and hence, you will not need alias

	//Load the previous alias from current proxy rules
	targetProxyEntry, err := dynamicProxyRouter.LoadProxy(rootNameOrMatchingDomain)
	if err != nil {
		utils.SendErrorResponse(w, "Target proxy config not found or could not be loaded")
		return
	}

	newAliasJSON, err := utils.PostPara(r, "alias")
	if err != nil {
		//No new set of alias given
		utils.SendErrorResponse(w, "new alias not given")
		return
	}

	//Write new alias to runtime and file
	newAlias := []string{}
	err = json.Unmarshal([]byte(newAliasJSON), &newAlias)
	if err != nil {
		SystemWideLogger.PrintAndLog("Proxy", "Unable to parse new alias list", err)
		utils.SendErrorResponse(w, "Invalid alias list given")
		return
	}

	//Set the current alias
	newProxyEndpoint := dynamicproxy.CopyEndpoint(targetProxyEntry)
	newProxyEndpoint.MatchingDomainAlias = newAlias

	// Prepare to replace the current routing rule
	readyRoutingRule, err := dynamicProxyRouter.PrepareProxyRoute(newProxyEndpoint)
	if err != nil {
		utils.SendErrorResponse(w, err.Error())
		return
	}
	targetProxyEntry.Remove()
	dynamicProxyRouter.AddProxyRouteToRuntime(readyRoutingRule)

	// Save it to file
	err = SaveReverseProxyConfig(newProxyEndpoint)
	if err != nil {
		utils.SendErrorResponse(w, "Alias update failed")
		SystemWideLogger.PrintAndLog("Proxy", "Unable to save alias update", err)
	}

	utils.SendOK(w)
}

func DeleteProxyEndpoint(w http.ResponseWriter, r *http.Request) {
	ep, err := utils.GetPara(r, "ep")
	if err != nil {
		utils.SendErrorResponse(w, "Invalid ep given")
		return
	}

	//Remove the config from runtime
	err = dynamicProxyRouter.RemoveProxyEndpointByRootname(ep)
	if err != nil {
		utils.SendErrorResponse(w, err.Error())
		return
	}

	//Remove the config from file
	err = RemoveReverseProxyConfig(ep)
	if err != nil {
		utils.SendErrorResponse(w, err.Error())
		return
	}

	//Update utm if exists
	if uptimeMonitor != nil {
		uptimeMonitor.Config.Targets = GetUptimeTargetsFromReverseProxyRules(dynamicProxyRouter)
		uptimeMonitor.CleanRecords()
	}

	//Update uptime monitor
	UpdateUptimeMonitorTargets()

	utils.SendOK(w)
}

/*
Handle update request for basic auth credential
Require paramter: ep (Endpoint) and pytype (proxy Type)
if request with GET, the handler will return current credentials
on this endpoint by its username

if request is POST, the handler will write the results to proxy config
*/
func UpdateProxyBasicAuthCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		ep, err := utils.GetPara(r, "ep")
		if err != nil {
			utils.SendErrorResponse(w, "Invalid ep given")
			return
		}

		//Load the target proxy object from router
		targetProxy, err := dynamicProxyRouter.LoadProxy(ep)
		if err != nil {
			utils.SendErrorResponse(w, err.Error())
			return
		}

		usernames := []string{}
		for _, cred := range targetProxy.BasicAuthCredentials {
			usernames = append(usernames, cred.Username)
		}

		js, _ := json.Marshal(usernames)
		utils.SendJSONResponse(w, string(js))

	} else if r.Method == http.MethodPost {
		//Write to target
		ep, err := utils.PostPara(r, "ep")
		if err != nil {
			utils.SendErrorResponse(w, "Invalid ep given")
			return
		}

		creds, err := utils.PostPara(r, "creds")
		if err != nil {
			utils.SendErrorResponse(w, "Invalid ptype given")
			return
		}

		//Load the target proxy object from router
		targetProxy, err := dynamicProxyRouter.LoadProxy(ep)
		if err != nil {
			utils.SendErrorResponse(w, err.Error())
			return
		}

		//Try to marshal the content of creds into the suitable structure
		newCredentials := []*dynamicproxy.BasicAuthUnhashedCredentials{}
		err = json.Unmarshal([]byte(creds), &newCredentials)
		if err != nil {
			utils.SendErrorResponse(w, "Malformed credential data")
			return
		}

		//Merge the credentials into the original config
		//If a new username exists in old config with no pw given, keep the old pw hash
		//If a new username is found with new password, hash it and push to credential slice
		mergedCredentials := []*dynamicproxy.BasicAuthCredentials{}
		for _, credential := range newCredentials {
			if credential.Password == "" {
				//Check if exists in the old credential files
				keepUnchange := false
				for _, oldCredEntry := range targetProxy.BasicAuthCredentials {
					if oldCredEntry.Username == credential.Username {
						//Exists! Reuse the old hash
						mergedCredentials = append(mergedCredentials, &dynamicproxy.BasicAuthCredentials{
							Username:     oldCredEntry.Username,
							PasswordHash: oldCredEntry.PasswordHash,
						})
						keepUnchange = true
					}
				}

				if !keepUnchange {
					//This is a new username with no pw given
					utils.SendErrorResponse(w, "Access password for "+credential.Username+" is empty!")
					return
				}
			} else {
				//This username have given password
				mergedCredentials = append(mergedCredentials, &dynamicproxy.BasicAuthCredentials{
					Username:     credential.Username,
					PasswordHash: auth.Hash(credential.Password),
				})
			}
		}

		targetProxy.BasicAuthCredentials = mergedCredentials

		//Save it to file
		SaveReverseProxyConfig(targetProxy)

		//Replace runtime configuration
		targetProxy.UpdateToRuntime()
		utils.SendOK(w)
	} else {
		http.Error(w, "invalid usage", http.StatusMethodNotAllowed)
	}

}

// List, Update or Remove the exception paths for basic auth.
func ListProxyBasicAuthExceptionPaths(w http.ResponseWriter, r *http.Request) {

	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
	ep, err := utils.GetPara(r, "ep")
	if err != nil {
		utils.SendErrorResponse(w, "Invalid ep given")
		return
	}

	//Load the target proxy object from router
	targetProxy, err := dynamicProxyRouter.LoadProxy(ep)
	if err != nil {
		utils.SendErrorResponse(w, err.Error())
		return
	}

	//List all the exception paths for this proxy
	results := targetProxy.BasicAuthExceptionRules
	if results == nil {
		//It is a config from a really old version of zoraxy. Overwrite it with empty array
		results = []*dynamicproxy.BasicAuthExceptionRule{}
	}
	js, _ := json.Marshal(results)
	utils.SendJSONResponse(w, string(js))
	return
}

func AddProxyBasicAuthExceptionPaths(w http.ResponseWriter, r *http.Request) {
	ep, err := utils.PostPara(r, "ep")
	if err != nil {
		utils.SendErrorResponse(w, "Invalid ep given")
		return
	}

	matchingPrefix, err := utils.PostPara(r, "prefix")
	if err != nil {
		utils.SendErrorResponse(w, "Invalid matching prefix given")
		return
	}

	//Load the target proxy object from router
	targetProxy, err := dynamicProxyRouter.LoadProxy(ep)
	if err != nil {
		utils.SendErrorResponse(w, err.Error())
		return
	}

	//Check if the prefix starts with /. If not, prepend it
	if !strings.HasPrefix(matchingPrefix, "/") {
		matchingPrefix = "/" + matchingPrefix
	}

	//Add a new exception rule if it is not already exists
	alreadyExists := false
	for _, thisExceptionRule := range targetProxy.BasicAuthExceptionRules {
		if thisExceptionRule.PathPrefix == matchingPrefix {
			alreadyExists = true
			break
		}
	}
	if alreadyExists {
		utils.SendErrorResponse(w, "This matching path already exists")
		return
	}
	targetProxy.BasicAuthExceptionRules = append(targetProxy.BasicAuthExceptionRules, &dynamicproxy.BasicAuthExceptionRule{
		PathPrefix: strings.TrimSpace(matchingPrefix),
	})

	//Save configs to runtime and file
	targetProxy.UpdateToRuntime()
	SaveReverseProxyConfig(targetProxy)

	utils.SendOK(w)
}

func RemoveProxyBasicAuthExceptionPaths(w http.ResponseWriter, r *http.Request) {
	// Delete a rule
	ep, err := utils.PostPara(r, "ep")
	if err != nil {
		utils.SendErrorResponse(w, "Invalid ep given")
		return
	}

	matchingPrefix, err := utils.PostPara(r, "prefix")
	if err != nil {
		utils.SendErrorResponse(w, "Invalid matching prefix given")
		return
	}

	// Load the target proxy object from router
	targetProxy, err := dynamicProxyRouter.LoadProxy(ep)
	if err != nil {
		utils.SendErrorResponse(w, err.Error())
		return
	}

	newExceptionRuleList := []*dynamicproxy.BasicAuthExceptionRule{}
	matchingExists := false
	for _, thisExceptionalRule := range targetProxy.BasicAuthExceptionRules {
		if thisExceptionalRule.PathPrefix != matchingPrefix {
			newExceptionRuleList = append(newExceptionRuleList, thisExceptionalRule)
		} else {
			matchingExists = true
		}
	}

	if !matchingExists {
		utils.SendErrorResponse(w, "target matching rule not exists")
		return
	}

	targetProxy.BasicAuthExceptionRules = newExceptionRuleList

	// Save configs to runtime and file
	targetProxy.UpdateToRuntime()
	SaveReverseProxyConfig(targetProxy)

	utils.SendOK(w)
}

// Report the current status of the reverse proxy server
func ReverseProxyStatus(w http.ResponseWriter, r *http.Request) {
	js, _ := json.Marshal(dynamicProxyRouter)
	utils.SendJSONResponse(w, string(js))
}

// Toggle a certain rule on and off
func ReverseProxyToggleRuleSet(w http.ResponseWriter, r *http.Request) {
	//No need to check for type as root cannot be turned off
	ep, err := utils.PostPara(r, "ep")
	if err != nil {
		utils.SendErrorResponse(w, "invalid ep given")
		return
	}

	targetProxyRule, err := dynamicProxyRouter.LoadProxy(ep)
	if err != nil {
		utils.SendErrorResponse(w, "invalid endpoint given")
		return
	}

	enableStr, err := utils.PostPara(r, "enable")
	if err != nil {
		enableStr = "true"
	}

	//Flip the enable and disabled tag state
	ruleDisabled := enableStr == "false"

	targetProxyRule.Disabled = ruleDisabled
	err = SaveReverseProxyConfig(targetProxyRule)
	if err != nil {
		utils.SendErrorResponse(w, "unable to save updated rule")
		return
	}
	utils.SendOK(w)
}

func ReverseProxyListDetail(w http.ResponseWriter, r *http.Request) {
	eptype, err := utils.PostPara(r, "type") //Support root and host
	if err != nil {
		utils.SendErrorResponse(w, "type not defined")
		return
	}

	if eptype == "host" {
		epname, err := utils.PostPara(r, "epname")
		if err != nil {
			utils.SendErrorResponse(w, "epname not defined")
			return
		}
		endpointRaw, ok := dynamicProxyRouter.ProxyEndpoints.Load(epname)
		if !ok {
			utils.SendErrorResponse(w, "proxy rule not found")
			return
		}
		targetEndpoint := dynamicproxy.CopyEndpoint(endpointRaw.(*dynamicproxy.ProxyEndpoint))
		js, _ := json.Marshal(targetEndpoint)
		utils.SendJSONResponse(w, string(js))
	} else if eptype == "root" {
		js, _ := json.Marshal(dynamicProxyRouter.Root)
		utils.SendJSONResponse(w, string(js))
	} else {
		utils.SendErrorResponse(w, "Invalid type given")
	}
}

func ReverseProxyList(w http.ResponseWriter, r *http.Request) {
	eptype, err := utils.PostPara(r, "type") //Support root and host
	if err != nil {
		utils.SendErrorResponse(w, "type not defined")
		return
	}

	if eptype == "host" {
		results := []*dynamicproxy.ProxyEndpoint{}
		dynamicProxyRouter.ProxyEndpoints.Range(func(key, value interface{}) bool {
			thisEndpoint := dynamicproxy.CopyEndpoint(value.(*dynamicproxy.ProxyEndpoint))

			//Clear the auth passwords before showing to front-end
			cleanedCredentials := []*dynamicproxy.BasicAuthCredentials{}
			for _, user := range thisEndpoint.BasicAuthCredentials {
				cleanedCredentials = append(cleanedCredentials, &dynamicproxy.BasicAuthCredentials{
					Username:     user.Username,
					PasswordHash: "",
				})
			}

			thisEndpoint.BasicAuthCredentials = cleanedCredentials
			results = append(results, thisEndpoint)
			return true
		})

		sort.Slice(results, func(i, j int) bool {
			return results[i].Domain < results[j].Domain
		})

		js, _ := json.Marshal(results)
		utils.SendJSONResponse(w, string(js))
	} else if eptype == "root" {
		js, _ := json.Marshal(dynamicProxyRouter.Root)
		utils.SendJSONResponse(w, string(js))
	} else {
		utils.SendErrorResponse(w, "Invalid type given")
	}
}

// Handle port 80 incoming traffics
func HandleUpdatePort80Listener(w http.ResponseWriter, r *http.Request) {
	enabled, err := utils.GetPara(r, "enable")
	if err != nil {
		//Load the current status
		currentEnabled := false
		err = sysdb.Read("settings", "listenP80", &currentEnabled)
		if err != nil {
			utils.SendErrorResponse(w, err.Error())
			return
		}
		js, _ := json.Marshal(currentEnabled)
		utils.SendJSONResponse(w, string(js))
	} else {
		if enabled == "true" {
			sysdb.Write("settings", "listenP80", true)
			SystemWideLogger.Println("Enabling port 80 listener")
			dynamicProxyRouter.UpdatePort80ListenerState(true)
		} else if enabled == "false" {
			sysdb.Write("settings", "listenP80", false)
			SystemWideLogger.Println("Disabling port 80 listener")
			dynamicProxyRouter.UpdatePort80ListenerState(false)
		} else {
			utils.SendErrorResponse(w, "invalid mode given: "+enabled)
		}
		utils.SendOK(w)
	}
}

// Handle https redirect
func HandleUpdateHttpsRedirect(w http.ResponseWriter, r *http.Request) {
	useRedirect, err := utils.GetPara(r, "set")
	if err != nil {
		currentRedirectToHttps := false
		//Load the current status
		err = sysdb.Read("settings", "redirect", &currentRedirectToHttps)
		if err != nil {
			utils.SendErrorResponse(w, err.Error())
			return
		}
		js, _ := json.Marshal(currentRedirectToHttps)
		utils.SendJSONResponse(w, string(js))
	} else {
		if dynamicProxyRouter.Option.Port == 80 {
			utils.SendErrorResponse(w, "This option is not available when listening on port 80")
			return
		}
		if useRedirect == "true" {
			sysdb.Write("settings", "redirect", true)
			SystemWideLogger.Println("Updating force HTTPS redirection to true")
			dynamicProxyRouter.UpdateHttpToHttpsRedirectSetting(true)
		} else if useRedirect == "false" {
			sysdb.Write("settings", "redirect", false)
			SystemWideLogger.Println("Updating force HTTPS redirection to false")
			dynamicProxyRouter.UpdateHttpToHttpsRedirectSetting(false)
		}

		utils.SendOK(w)
	}
}

// Handle checking if the current user is accessing via the reverse proxied interface
// Of the management interface.
func HandleManagementProxyCheck(w http.ResponseWriter, r *http.Request) {
	isProxied := dynamicProxyRouter.IsProxiedSubdomain(r)
	js, _ := json.Marshal(isProxied)
	utils.SendJSONResponse(w, string(js))
}

func HandleDevelopmentModeChange(w http.ResponseWriter, r *http.Request) {
	enableDevelopmentModeStr, err := utils.GetPara(r, "enable")
	if err != nil {
		//Load the current development mode toggle state
		js, _ := json.Marshal(dynamicProxyRouter.Option.NoCache)
		utils.SendJSONResponse(w, string(js))
	} else {
		//Write changes to runtime
		enableDevelopmentMode := false
		if enableDevelopmentModeStr == "true" {
			enableDevelopmentMode = true
		}

		//Write changes to runtime
		dynamicProxyRouter.Option.NoCache = enableDevelopmentMode

		//Write changes to database
		sysdb.Write("settings", "devMode", enableDevelopmentMode)

		utils.SendOK(w)
	}

}

// Handle incoming port set. Change the current proxy incoming port
func HandleIncomingPortSet(w http.ResponseWriter, r *http.Request) {
	newIncomingPort, err := utils.PostPara(r, "incoming")
	if err != nil {
		utils.SendErrorResponse(w, "invalid incoming port given")
		return
	}

	newIncomingPortInt, err := strconv.Atoi(newIncomingPort)
	if err != nil {
		utils.SendErrorResponse(w, "Invalid incoming port given")
		return
	}

	//Check if it is identical as proxy root (recursion!)
	if dynamicProxyRouter.Root == nil || dynamicProxyRouter.Root.Domain == "" {
		//Check if proxy root is set before checking recursive listen
		//Fixing issue #43
		utils.SendErrorResponse(w, "Set Proxy Root before changing inbound port")
		return
	}

	proxyRoot := strings.TrimSuffix(dynamicProxyRouter.Root.Domain, "/")
	if strings.EqualFold(proxyRoot, "localhost:"+strconv.Itoa(newIncomingPortInt)) || strings.EqualFold(proxyRoot, "127.0.0.1:"+strconv.Itoa(newIncomingPortInt)) {
		//Listening port is same as proxy root
		//Not allow recursive settings
		utils.SendErrorResponse(w, "Recursive listening port! Check your proxy root settings.")
		return
	}

	//Stop and change the setting of the reverse proxy service
	if dynamicProxyRouter.Running {
		dynamicProxyRouter.StopProxyService()
		dynamicProxyRouter.Option.Port = newIncomingPortInt
		dynamicProxyRouter.StartProxyService()
	} else {
		//Only change setting but not starting the proxy service
		dynamicProxyRouter.Option.Port = newIncomingPortInt
	}

	sysdb.Write("settings", "inbound", newIncomingPortInt)

	utils.SendOK(w)
}

/* Handle Custom Header Rules */
//List all the custom header defined in this proxy rule

func HandleCustomHeaderList(w http.ResponseWriter, r *http.Request) {
	epType, err := utils.PostPara(r, "type")
	if err != nil {
		utils.SendErrorResponse(w, "endpoint type not defined")
		return
	}

	domain, err := utils.PostPara(r, "domain")
	if err != nil {
		utils.SendErrorResponse(w, "domain or matching rule not defined")
		return
	}

	var targetProxyEndpoint *dynamicproxy.ProxyEndpoint
	if epType == "root" {
		targetProxyEndpoint = dynamicProxyRouter.Root
	} else {
		ep, err := dynamicProxyRouter.LoadProxy(domain)
		if err != nil {
			utils.SendErrorResponse(w, "target endpoint not exists")
			return
		}

		targetProxyEndpoint = ep
	}

	//List all custom headers
	customHeaderList := targetProxyEndpoint.UserDefinedHeaders
	if customHeaderList == nil {
		customHeaderList = []*dynamicproxy.UserDefinedHeader{}
	}
	js, _ := json.Marshal(customHeaderList)
	utils.SendJSONResponse(w, string(js))

}

// Add a new header to the target endpoint
func HandleCustomHeaderAdd(w http.ResponseWriter, r *http.Request) {
	rewriteType, err := utils.PostPara(r, "type")
	if err != nil {
		utils.SendErrorResponse(w, "rewriteType not defined")
		return
	}

	domain, err := utils.PostPara(r, "domain")
	if err != nil {
		utils.SendErrorResponse(w, "domain or matching rule not defined")
		return
	}

	direction, err := utils.PostPara(r, "direction")
	if err != nil {
		utils.SendErrorResponse(w, "HTTP modifiy direction not set")
		return
	}

	name, err := utils.PostPara(r, "name")
	if err != nil {
		utils.SendErrorResponse(w, "HTTP header name not set")
		return
	}

	value, err := utils.PostPara(r, "value")
	if err != nil && rewriteType == "add" {
		utils.SendErrorResponse(w, "HTTP header value not set")
		return
	}

	targetProxyEndpoint, err := dynamicProxyRouter.LoadProxy(domain)
	if err != nil {
		utils.SendErrorResponse(w, "target endpoint not exists")
		return
	}

	//Create a Custom Header Defination type
	var rewriteDirection dynamicproxy.HeaderDirection
	if direction == "toOrigin" {
		rewriteDirection = dynamicproxy.HeaderDirection_ZoraxyToUpstream
	} else if direction == "toClient" {
		rewriteDirection = dynamicproxy.HeaderDirection_ZoraxyToDownstream
	} else {
		//Unknown direction
		utils.SendErrorResponse(w, "header rewrite direction not supported")
		return
	}

	isRemove := false
	if rewriteType == "remove" {
		isRemove = true
	}
	headerRewriteDefination := dynamicproxy.UserDefinedHeader{
		Key:       name,
		Value:     value,
		Direction: rewriteDirection,
		IsRemove:  isRemove,
	}

	//Create a new custom header object
	err = targetProxyEndpoint.AddUserDefinedHeader(&headerRewriteDefination)
	if err != nil {
		utils.SendErrorResponse(w, "unable to add header rewrite rule: "+err.Error())
		return
	}

	//Save it (no need reload as header are not handled by dpcore)
	err = SaveReverseProxyConfig(targetProxyEndpoint)
	if err != nil {
		utils.SendErrorResponse(w, "unable to save update")
		return
	}

	utils.SendOK(w)
}

// Remove a header from the target endpoint
func HandleCustomHeaderRemove(w http.ResponseWriter, r *http.Request) {
	domain, err := utils.PostPara(r, "domain")
	if err != nil {
		utils.SendErrorResponse(w, "domain or matching rule not defined")
		return
	}

	name, err := utils.PostPara(r, "name")
	if err != nil {
		utils.SendErrorResponse(w, "HTTP header name not set")
		return
	}

	targetProxyEndpoint, err := dynamicProxyRouter.LoadProxy(domain)
	if err != nil {
		utils.SendErrorResponse(w, "target endpoint not exists")
		return
	}

	err = targetProxyEndpoint.RemoveUserDefinedHeader(name)
	if err != nil {
		utils.SendErrorResponse(w, "unable to remove header rewrite rule: "+err.Error())
		return
	}

	err = SaveReverseProxyConfig(targetProxyEndpoint)
	if err != nil {
		utils.SendErrorResponse(w, "unable to save update")
		return
	}

	utils.SendOK(w)

}

// Handle view or edit HSTS states
func HandleHSTSState(w http.ResponseWriter, r *http.Request) {
	domain, err := utils.PostPara(r, "domain")
	if err != nil {
		domain, err = utils.GetPara(r, "domain")
		if err != nil {
			utils.SendErrorResponse(w, "domain or matching rule not defined")
			return
		}
	}

	targetProxyEndpoint, err := dynamicProxyRouter.LoadProxy(domain)
	if err != nil {
		utils.SendErrorResponse(w, "target endpoint not exists")
		return
	}

	if r.Method == http.MethodGet {
		//Return current HSTS enable state
		hstsAge := targetProxyEndpoint.HSTSMaxAge
		js, _ := json.Marshal(hstsAge)
		utils.SendJSONResponse(w, string(js))
		return
	} else if r.Method == http.MethodPost {
		newMaxAge, err := utils.PostInt(r, "maxage")
		if err != nil {
			utils.SendErrorResponse(w, "maxage not defeined")
			return
		}

		if newMaxAge == 0 || newMaxAge >= 31536000 {
			targetProxyEndpoint.HSTSMaxAge = int64(newMaxAge)
			SaveReverseProxyConfig(targetProxyEndpoint)
			targetProxyEndpoint.UpdateToRuntime()
		} else {
			utils.SendErrorResponse(w, "invalid max age given")
			return
		}
		utils.SendOK(w)
		return
	}

	http.Error(w, "405 - Method not allowed", http.StatusMethodNotAllowed)
}

// HandlePermissionPolicy handle read or write to permission policy
func HandlePermissionPolicy(w http.ResponseWriter, r *http.Request) {
	domain, err := utils.PostPara(r, "domain")
	if err != nil {
		domain, err = utils.GetPara(r, "domain")
		if err != nil {
			utils.SendErrorResponse(w, "domain or matching rule not defined")
			return
		}
	}

	targetProxyEndpoint, err := dynamicProxyRouter.LoadProxy(domain)
	if err != nil {
		utils.SendErrorResponse(w, "target endpoint not exists")
		return
	}

	if r.Method == http.MethodGet {
		type CurrentPolicyState struct {
			PPEnabled     bool
			CurrentPolicy *permissionpolicy.PermissionsPolicy
		}

		currentPolicy := permissionpolicy.GetDefaultPermissionPolicy()
		if targetProxyEndpoint.PermissionPolicy != nil {
			currentPolicy = targetProxyEndpoint.PermissionPolicy
		}
		result := CurrentPolicyState{
			PPEnabled:     targetProxyEndpoint.EnablePermissionPolicyHeader,
			CurrentPolicy: currentPolicy,
		}

		js, _ := json.Marshal(result)
		utils.SendJSONResponse(w, string(js))
		return
	} else if r.Method == http.MethodPost {
		//Update the enable state of permission policy
		enableState, err := utils.PostBool(r, "enable")
		if err != nil {
			utils.SendErrorResponse(w, "invalid enable state given")
			return
		}

		targetProxyEndpoint.EnablePermissionPolicyHeader = enableState
		SaveReverseProxyConfig(targetProxyEndpoint)
		targetProxyEndpoint.UpdateToRuntime()
		utils.SendOK(w)
		return
	} else if r.Method == http.MethodPut {
		//Store the new permission policy
		newPermissionPolicyJSONString, err := utils.PostPara(r, "pp")
		if err != nil {
			utils.SendErrorResponse(w, "missing pp (permission policy) paramter")
			return
		}

		//Parse the permission policy from JSON string
		newPermissionPolicy := permissionpolicy.GetDefaultPermissionPolicy()
		err = json.Unmarshal([]byte(newPermissionPolicyJSONString), &newPermissionPolicy)
		if err != nil {
			utils.SendErrorResponse(w, "permission policy parse error: "+err.Error())
			return
		}

		//Save it to file
		targetProxyEndpoint.PermissionPolicy = newPermissionPolicy
		SaveReverseProxyConfig(targetProxyEndpoint)
		targetProxyEndpoint.UpdateToRuntime()
		utils.SendOK(w)
		return
	}

	http.Error(w, "405 - Method not allowed", http.StatusMethodNotAllowed)
}
