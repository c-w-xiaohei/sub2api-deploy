package main

type EdgeTriggerInput struct {
	TraefikImage  string
	ACMEEmail     string
	SingBoxConfig string
	EdgeChecksum  string
}

func BuildEdgeTriggers(input EdgeTriggerInput) []string {
	return []string{"edge-reconcile-v1", input.TraefikImage, input.ACMEEmail, input.SingBoxConfig, input.EdgeChecksum}
}

type SiteTriggerInput struct {
	SiteID         string
	Domain         string
	RuntimeRoot    string
	ComposeProject string
	RoutePath      string
	SiteChecksum   string
}

func BuildSiteReconcileTriggers(input SiteTriggerInput) []string {
	return []string{"site-reconcile-v1", input.SiteID, input.Domain, input.RuntimeRoot, input.ComposeProject, input.RoutePath, input.SiteChecksum}
}

func BuildSiteReleaseTriggers(siteID, image string) []string {
	return []string{"site-release-v1", siteID, image}
}
