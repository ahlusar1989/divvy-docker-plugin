package main

import (
    "io/ioutil"
    "log"
    "os"
    "path/filepath"
    "strings"

    "github.com/alecthomas/kingpin"
    "github.com/aws/aws-sdk-go/aws"
    "github.com/aws/aws-sdk-go/aws/ec2metadata"
    "github.com/aws/aws-sdk-go/service/ec2"
    "github.com/aws/aws-sdk-go/service/efs"
    "github.com/docker/docker/pkg/mount"
    "github.com/jasonlvhit/gocron"
)

const (
    pluginId = "efs"
)

var (
    socketAddress = filepath.Join("/run/docker/plugins/", strings.Join([]string{pluginId, ".sock"}, ""))
    defaultDir    = filepath.Join(dkvolume.DefaultDockerRootDirectory, pluginId)

    // CLI Arguments.
    cliRoot     = kingpin.Flag("root", "EFS volumes root directory.").Default(defaultDir).String()
    cliSecurity = kingpin.Flag("security", "Security group to be assigned to new EFS Mount points.").Default("").OverrideDefaultFromEnvar("DOCKER_VOLUMES_EFS_SECURITY").String()
    cliVerbose  = kingpin.Flag("verbose", "Show verbose logging.").Bool()
)


func main() {
    kingpin.Parse()

    // This is a scheduled set of tasks which will unmount old directories which
    // are not being used by container instances.
    gocron.Every(15).Seconds().Do(Cleanup, *cliRoot)
    go gocron.Start()

    // Discovery the region which this instance resides. This will ensure the
    // EFS Filesystem gets created in the same region as this instance.
    metadata := ec2metadata.New(&ec2metadata.Config{})
    region, err := metadata.Region()
    if err != nil {
        panic(err)
    }

    // We need to determine which region this host lives in. That will allow us to spin
    // up EFS Filesystem within this region.
    e := ec2.New(&aws.Config{Region: aws.String(region)})

    i, err := metadata.GetMetadata("instance-id")
    if err != nil {
        panic(err)
    }

    subnet, err := GetSubnet(e, i)
    if err != nil {
        panic(err)
    }

    d := DriverEFS{
        Root:   *cliRoot,
        Region: region,
        Subnet: subnet,
    }
    h := dkvolume.NewHandler(d)
    log.Printf("Listening: %s", socketAddress)
    log.Println(h.ServeUnix("root", socketAddress))
}

func Cleanup(d string) {
    log.Println("Running cleanup task")

    // Get a list of all the current running containers.
    mounts, err := GetDockerBinds()
    if err != nil {
        log.Println(err)
        return
    }

    // Go over the list of possible mounts and compare against the Docker running
    // containers list.
    files, _ := ioutil.ReadDir(d + "/")
    for _, f := range files {
        m := f.Name()
        p := filepath.Join(d, m)

        // We only deal with directories.
        if !f.IsDir() {
            continue
        }

        // We only deal with directories which are also mounts.
        nfs, err := mount.Mounted(p)
        if err != nil {
            log.Printf("Cannot determine if mounted %s", m)
            continue
        }
        if !nfs {
            continue
        }

        // Ensure that we are not unmounting filesystems which are still
        // being used by a container in Docker.
        if Contains(mounts, m) {
            continue
        }

        err = Exec("umount", p)
        if err != nil {
            log.Printf("Cleanup failed: %s", m)
            return
        }
        log.Printf("Cleaned: %s", m)
    }
}