package kamsuite

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"github.com/cucumber/messages-go/v10"
	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/go-scm/scm/factory"
	"github.com/redhat-developer/kam/pkg/pipelines/yaml"
	"k8s.io/apimachinery/pkg/util/wait"

	deployment "github.com/redhat-developer/kam/pkg/pipelines/deployment"
	"github.com/redhat-developer/kam/pkg/pipelines/git"
	"github.com/redhat-developer/kam/pkg/pipelines/ioutils"
	res "github.com/redhat-developer/kam/pkg/pipelines/resources"
)

// FeatureContext defines godog.Suite steps for the test suite.
func FeatureContext(s *godog.Suite) {

	// KAM related steps
	s.Step(`^directory "([^"]*)" should exist$`,
		DirectoryShouldExist)

	// For creating repository with name specified in basic.feature
	s.Step(`^"([^"]*)" repository is created$`,
		createRepository)

	s.Step(`^Wait for application "([^"]*)" to be in "([^"]*)" state$`,
		waitSync)

	// Adding sample kubernetes resource
	s.Step(`^Add kubernetes resource to the service in new environment$`,
		addResource)

	s.Step(`^Create a pull request$`,
		createPR)

	s.Step(`^Wait for all the checks to pass and merge the pull request$`,
		waitPass)

	s.BeforeSuite(func() {
		fmt.Println("Before suite")
		if !envVariableCheck() {
			os.Exit(1)
		}
		err := loginToArgoAPIServer()
		if err != nil {
			log.Fatal(err)
		}
	})

	s.AfterSuite(func() {
		fmt.Println("After suite")
	})

	s.BeforeFeature(func(this *messages.GherkinDocument) {
		fmt.Println("Before feature")
	})

	s.AfterFeature(func(this *messages.GherkinDocument) {
		fmt.Println("After feature")
	})

	s.BeforeScenario(func(this *messages.Pickle) {
		fmt.Println("Before scenario")

		// Clearing working directory before each scenario
		f, err := os.Open("./")
		if err == nil {
			err := os.RemoveAll("bootstrapresources")
			if err != nil {
				log.Fatal(err)
			}
			err2 := os.RemoveAll("secrets")
			if err2 != nil {
				log.Fatal(err2)
			}
			fmt.Println("Cleared working directory")
		}
		defer f.Close()
	})

	s.AfterScenario(func(*messages.Pickle, error) {
		fmt.Println("After scenario")
		re := regexp.MustCompile(`[a-z]+`)
		scm := re.FindAllString(os.Getenv("GITOPS_REPO_URL"), 2)[1]

		switch scm {
		case "github":
			deleteGithubRepository(os.Getenv("GITOPS_REPO_URL"), os.Getenv("GIT_ACCESS_TOKEN"))
		case "gitlab":
			deleteGitlabRepoStep := []string{"repo", "delete", strings.Split(strings.Split(os.Getenv("GITOPS_REPO_URL"), ".com/")[1], ".")[0], "-y"}
			ok, errMessage := deleteGitlabRepository(deleteGitlabRepoStep)
			if !ok {
				fmt.Println(errMessage)
			}
		default:
			fmt.Println("SCM is not supported")
		}
	})
}

func envVariableCheck() bool {
	envVars := []string{"SERVICE_REPO_URL", "GITOPS_REPO_URL", "IMAGE_REPO", "DOCKERCONFIGJSON_PATH", "GIT_ACCESS_TOKEN", "BUS_REPO_URL"}
	val, ok := os.LookupEnv("CI")
	if !ok {
		for _, envVar := range envVars {
			_, ok := os.LookupEnv(envVar)
			if !ok {
				fmt.Printf("%s is not set\n", envVar)
				return false
			}
		}

		re := regexp.MustCompile(`[a-z]+`)
		scm := re.FindAllString(os.Getenv("GITOPS_REPO_URL"), 2)[1]

		switch scm {
		case "github":
			os.Setenv("GITHUB_TOKEN", os.Getenv("GIT_ACCESS_TOKEN"))
		case "gitlab":
			os.Setenv("GITLAB_TOKEN", os.Getenv("GIT_ACCESS_TOKEN"))
		default:
			fmt.Println("SCM is not supported")
		}
	} else {
		if val == "prow" {
			fmt.Printf("Running e2e test in OpenShift CI\n")
			majorVersion, err := openhiftServerVersion()
			if err != nil {
				fmt.Printf("OpenShift API server version not found\n")
				return false
			}
			os.Setenv("SERVICE_REPO_URL", "https://github.com/kam-bot/taxi")
			os.Setenv("GITOPS_REPO_URL", "https://github.com/kam-bot/taxi-"+os.Getenv("PRNO")+majorVersion)
			os.Setenv("IMAGE_REPO", "quay.io/kam-bot/taxi")
			os.Setenv("BUS_REPO_URL", "https://github.com/kam-bot/bus")
			os.Setenv("DOCKERCONFIGJSON_PATH", os.Getenv("KAM_QUAY_DOCKER_CONF_SECRET_FILE"))
			os.Setenv("GIT_ACCESS_TOKEN", os.Getenv("GITHUB_TOKEN"))
		} else {
			fmt.Printf("You cannot run e2e test locally against OpenShift CI\n")
			return false
		}
		return true
	}
	return true
}

func deleteGitlabRepository(arg []string) (bool, string) {
	var stderr bytes.Buffer
	cmd := exec.Command("glab", arg...)
	fmt.Println("gitlab command is : ", cmd.Args)
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return false, stderr.String()
	}
	return true, stderr.String()
}

func deleteGithubRepository(repoURL, token string) {
	repo, err := git.NewRepository(repoURL, token)
	if err != nil {
		log.Fatal(err)
	}
	parsed, err := url.Parse(repoURL)
	if err != nil {
		log.Fatalf("failed to parse repository URL %q: %v", repoURL, err)
	}
	repoName, err := git.GetRepoName(parsed)
	if err != nil {
		log.Fatal(err)
	}
	_, err = repo.Repositories.Delete(context.TODO(), repoName)
	if err != nil {
		log.Printf("unable to delete repository %v: %v", repoName, err)
	} else {
		log.Printf("Successfully deleted repository: %q", repoURL)
	}
}

func waitSync(app string, state string) error {
	err := wait.Poll(time.Second*1, time.Minute*10, func() (bool, error) {
		return argoAppStatusMatch(state, app)
	})
	if err != nil {
		return fmt.Errorf("error is : %v", err)
	}
	return nil
}

func parse(repo string) (string, *scm.Client, error) {
	parsed, err := url.Parse(os.Getenv("GITOPS_REPO_URL"))
	if err != nil {
		return "", nil, err
	}
	name := strings.Split(os.Getenv("GITOPS_REPO_URL"), "/")
	parsed.User = url.UserPassword("", os.Getenv("GITHUB_TOKEN"))
	client, err := factory.FromRepoURL(parsed.String())
	if err != nil {
		return "", nil, err
	}
	return name[4], client, err
}

func createRepository(repo string) error {
	name, client, err := parse(repo)
	if err != nil {
		return err
	}

	ri := &scm.RepositoryInput{
		Private:     false,
		Description: "repocreate",
		Namespace:   "",
		Name:        name,
	}

	_, _, err = client.Repositories.Create(context.Background(), ri)
	if err != nil {
		return err
	}
	fmt.Printf("Created repositry: %v\n", name)
	return nil
}

func createPR() error {
	_, client, err := parse(strings.Split(os.Getenv("GITOPS_REPO_URL"), "/")[4])
	if err != nil {
		return err
	}

	pr := &scm.PullRequestInput{
		Title: "Add new service",
		Head:  "addNewService",
		Base:  "main",
		Body:  "Add new service",
	}

	_, _, err = client.PullRequests.Create(context.Background(), strings.Split(os.Getenv("GITOPS_REPO_URL"), ".com/")[1], pr)
	if err != nil {
		return err
	}
	fmt.Println("PR has been created")
	return nil
}

func mergePR() error {
	_, client, err := parse(strings.Split(os.Getenv("GITOPS_REPO_URL"), "/")[4])
	if err != nil {
		return err
	}

	merge := &scm.PullRequestMergeOptions{
		DeleteSourceBranch: true,
	}

	_, err = client.PullRequests.Merge(context.Background(), strings.Split(os.Getenv("GITOPS_REPO_URL"), ".com/")[1], 1, merge)
	if err != nil {
		return err
	}
	fmt.Println("PR has been merged!")
	return nil
}

func waitPass() error {

	var stderr bytes.Buffer
	var stdout bytes.Buffer

	ocPath, err := executableBinaryPath("oc")
	if err != nil {
		return err
	}

	err = wait.Poll(time.Second*1, time.Minute*30, func() (bool, error) {

		cmd := exec.Command(ocPath, "get", "pipelinerun", "-n", "cicd")
		cmd.Stderr = &stderr
		cmd.Stdout = &stdout
		err = cmd.Run()

		if err != nil {
			return false, err
		}

		if stdout.String() == "" {
			if strings.Contains(stderr.String(), "No resources found in cicd namespace.") {
				return false, nil
			}
		}
		return true, nil
	})

	if err != nil {
		return err
	}

	fmt.Println("Waiting for checks to pass")
	err = wait.Poll(time.Second*1, time.Minute*20, func() (bool, error) {

		output, err := exec.Command(ocPath, "get", "pipelinerun", "-n", "cicd", "--sort-by=.status.startTime", "-o", "jsonpath='{.items[-1].status.conditions[0].status}'").Output()
		if err != nil {
			fmt.Println("Error:", err)
			return false, err
		}

		if string(output) == "'Unknown'" {
			fmt.Print(".")
			return false, nil
		} else if string(output) == "'True'" {
			fmt.Println("\nPR is ready to be merged, all checks passed!")
			return true, nil
		} else {
			fmt.Println("\nOne or more checks failed!")
			return false, err
		}
	})

	if err != nil {
		return err
	} else {
		mergePR()
	}
	return nil
}

func loginToArgoAPIServer() error {
	var stderr bytes.Buffer
	argocdPath, err := executableBinaryPath("argocd")
	if err != nil {
		return err
	}

	argocdServer, err := argocdAPIServer()
	if err != nil {
		return err
	}

	argocdPassword, err := argocdAPIServerPassword()
	if err != nil {
		return err
	}

	cmd := exec.Command(argocdPath, "login", "--username", "admin", "--password", argocdPassword, argocdServer, "--grpc-web", "--insecure")
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		return err
	}
	fmt.Println("Logged in to ArgoCD API server successfully")
	return nil
}

func executableBinaryPath(executable string) (string, error) {
	path, err := exec.LookPath(executable)
	if err != nil {
		return "", err
	}
	return path, nil
}

func argocdAPIServer() (string, error) {
	var stdout bytes.Buffer

	ocPath, err := executableBinaryPath("oc")
	if err != nil {
		return "", err
	}

	deployments := []string{"openshift-gitops-server", "openshift-gitops-repo-server",
		"openshift-gitops-redis", "openshift-gitops-applicationset-controller", "kam", "cluster"}

	for index := range deployments {
		err = wait.Poll(time.Second*1, time.Minute*10, func() (bool, error) {
			return waitForDeploymentsUpAndRunning("openshift-gitops", deployments[index])
		})
	}

	if err != nil {
		return "", err
	}

	cmd := exec.Command(ocPath, "get", "routes", "-n", "openshift-gitops",
		"-o", "jsonpath='{.items[?(@.metadata.name==\"openshift-gitops-server\")].spec.host}'")
	cmd.Stdout = &stdout
	err = cmd.Run()
	if err != nil {
		return "", err
	}
	return strings.Trim(stdout.String(), "'"), nil
}

func argocdAPIServerPassword() (string, error) {
	var stdout, stderr bytes.Buffer
	ocPath, err := executableBinaryPath("oc")
	if err != nil {
		return "", err
	}

	cmd := exec.Command(ocPath, "get", "secret", "openshift-gitops-cluster", "-n", "openshift-gitops", "-o", "jsonpath='{.data.admin\\.password}'")

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		return "", err
	}

	data, err := base64.StdEncoding.DecodeString(strings.Trim(stdout.String(), "'"))
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func waitForDeploymentsUpAndRunning(namespace string, deploymentName string) (bool, error) {
	var stderr, stdout bytes.Buffer
	ocPath, err := executableBinaryPath("oc")
	if err != nil {
		return false, err
	}
	cmd := exec.Command(ocPath, "rollout", "status", "deployment", deploymentName, "-n", namespace)

	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	err = cmd.Run()
	if err == nil {
		if strings.Contains(stdout.String(), "successfully rolled out") {
			return true, err
		}
		return false, err
	}
	return false, err
}

func argoAppStatusMatch(matchString string, appName string) (bool, error) {
	var stdout bytes.Buffer
	argocdPath, err := executableBinaryPath("argocd")
	if err != nil {
		return false, err
	}

	appList := []string{"app", "list"}
	cmd := exec.Command(argocdPath, appList...)
	cmd.Stdout = &stdout
	if err = cmd.Run(); err != nil {
		return false, err
	}

	re, _ := regexp.Compile(appName + ".+")
	appDetailsString := re.FindString(stdout.String())
	if appDetailsString == " " {
		return false, nil
	}
	if strings.Contains(appDetailsString, matchString) {
		return true, nil
	}
	return false, nil
}

func openhiftServerVersion() (string, error) {
	var stdout bytes.Buffer
	ocPath, err := executableBinaryPath("oc")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(ocPath, "version")
	cmd.Stdout = &stdout
	if err = cmd.Run(); err != nil {
		return "", err
	}

	re := regexp.MustCompile(`Server\s+Version:\s+(\d.{2})`)
	return strings.Replace(strings.Trim(re.FindStringSubmatch(stdout.String())[1], "\""), ".", "", -1), nil
}

func addResource() error {
	appFs := ioutils.NewFilesystem()
	const (
		path           = "bootstrapresources/environments/new-env/apps/app-bus/services/bus/base/config"
		partOf         = "app-bus"
		env            = "new-env"
		name           = "bus"
		bootstrapImage = "nginxinc/nginx-unprivileged:latest"
	)
	resources := res.Resources{}
	resources[filepath.Join(path, "deployment.yaml")] = deployment.Create(partOf, env, name, bootstrapImage, deployment.ContainerPort(8080))
	resources[filepath.Join(path, "kustomization.yaml")] = &res.Kustomization{
		Resources: []string{
			"deployment.yaml",
		}}
	_, err := yaml.WriteResources(appFs, "", resources)
	return err
}
