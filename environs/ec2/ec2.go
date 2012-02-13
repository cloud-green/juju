package ec2

import (
	"fmt"
	"launchpad.net/goamz/ec2"
	"launchpad.net/goamz/s3"
	"launchpad.net/juju/go/environs"
	"launchpad.net/juju/go/state"
	"strings"
	"sync"
	"time"
"log"
)

const zkPort = 2181
var zkPortSuffix = fmt.Sprintf(":%d", zkPort)

func init() {
	environs.RegisterProvider("ec2", environProvider{})
}

type environProvider struct{}

var _ environs.EnvironProvider = environProvider{}

type environ struct {
	name             string
	config           *providerConfig
	ec2              *ec2.EC2
	s3               *s3.S3
	checkBucket      sync.Once
	checkBucketError error
}

var _ environs.Environ = (*environ)(nil)

type instance struct {
	*ec2.Instance
}

func (inst *instance) String() string {
	return inst.Id()
}

var _ environs.Instance = (*instance)(nil)

func (inst *instance) Id() string {
	return inst.InstanceId
}

func (inst *instance) DNSName() string {
	return inst.Instance.DNSName
}

func (environProvider) Open(name string, config interface{}) (e environs.Environ, err error) {
	cfg := config.(*providerConfig)
	if Regions[cfg.region].EC2Endpoint == "" {
		return nil, fmt.Errorf("no ec2 endpoint found for region %q, opening %q", cfg.region, name)
	}
	return &environ{
		name:   name,
		config: cfg,
		ec2:    ec2.New(cfg.auth, Regions[cfg.region]),
		s3:     s3.New(cfg.auth, Regions[cfg.region]),
	}, nil
}

func (e *environ) Bootstrap() (*state.Info, error) {
	_, err := e.loadState()
	if err == nil {
		return nil, fmt.Errorf("environment is already bootstrapped")
	}
	if s3err, _ := err.(*s3.Error); s3err != nil && s3err.StatusCode != 404 {
		return nil, err
	}
	inst, err := e.startInstance(0, nil, true)
	if err != nil {
		return nil, fmt.Errorf("cannot start bootstrap instance: %v", err)
	}
	err = e.saveState(&bootstrapState{
		ZookeeperInstances: []string{inst.Id()},
	})
	if err != nil {
		// ignore error on StopInstance because the previous error is
		// more important.
		e.StopInstances([]environs.Instance{inst})
		return nil, err
	}
	// try a few times to get the dns name of the new instance.
	for i := 0; i < 20; i++ {
		if inst.DNSName() != "" {
			break
		}
		time.Sleep(5e9)
		insts, err := e.Instances()
		if err != nil {
			// TODO perhaps we should return nil, nil here
			// as we have successfully bootstrapped - we just
			// can't get the DNS address of the bootstrapped instance.
			return nil, err
		}
		found := false
		for _, x := range insts {
			if x.Id() == inst.Id() {
				inst = x
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("cannot find just-started bootstrap instance %q", inst.Id())
		}
	}
	if inst.DNSName() == "" {
		return nil, fmt.Errorf("timed out trying to get bootstrap instance DNS address")
	}
	
	// TODO make safe in the case of racing Bootstraps
	// If two Bootstraps are called concurrently, there's
	// no way to use S3 to make sure that only one succeeds.
	// Perhaps consider using SimpleDB for state storage
	// which would enable that possibility.
	return &state.Info{[]string{zkAddr(inst.(*instance).Instance)}}, nil
}

func (e *environ) StateInfo() (*state.Info, error) {
	st, err := e.loadState()
	if err != nil {
		return nil, err
	}
	f := ec2.NewFilter()
	f.Add("instance-id", st.ZookeeperInstances...)
	resp, err := e.ec2.Instances(nil, f)
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, r := range resp.Reservations {
		for _, inst := range r.Instances {
			addrs = append(addrs, zkAddr(&inst))
		}
	}
	return &state.Info{addrs}, nil
}

func zkAddr(inst *ec2.Instance) string {
	return inst.DNSName + zkPortSuffix
}

func (e *environ) StartInstance(machineId int, info *state.Info) (environs.Instance, error) {
	return e.startInstance(machineId, info, false)
}

func (e *environ) userData(machineId int, info *state.Info, master bool) ([]byte, error) {
	cfg := &machineConfig{
		provisioner:        master,
		zookeeper:          master,
		stateInfo:          info,
		instanceIdAccessor: "$(curl http://169.254.169.254/1.0/meta-data/instance-id)",
		providerType:       "ec2",
		origin:             jujuOrigin{originBranch, "lp:jujubranch"},
		machineId:          fmt.Sprint(machineId),
	}

	var err error
	cfg.authorizedKeys, err = authorizedKeys(e.config.authorizedKeys, e.config.authorizedKeysPath)
	if err != nil {
		return nil, fmt.Errorf("cannot get ssh authorized keys: %v", err)
	}
	cloudcfg, err := newCloudInit(cfg)
	if err != nil {
		return nil, err
	}
	return cloudcfg.Render()
}

// startInstance is the internal version of StartInstance, used by Bootstrap
// as well as via StartInstance itself. If master is true, a bootstrap
// instance will be started.
func (e *environ) startInstance(machineId int, info *state.Info, master bool) (environs.Instance, error) {
	image, err := FindImageSpec(DefaultImageConstraint)
	if err != nil {
		return nil, fmt.Errorf("cannot find image: %v", err)
	}
	userData, err := e.userData(machineId, info, master)
	if err != nil {
		return nil, err
	}
	groups, err := e.setUpGroups(machineId)
	if err != nil {
		return nil, fmt.Errorf("cannot set up groups: %v", err)
	}
	instances, err := e.ec2.RunInstances(&ec2.RunInstances{
		ImageId:        image.ImageId,
		MinCount:       1,
		MaxCount:       1,
		UserData:       userData,
		InstanceType:   "m1.small",
		SecurityGroups: groups,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot run instances: %v", err)
	}
	if len(instances.Instances) != 1 {
		return nil, fmt.Errorf("expected 1 started instance, got %d", len(instances.Instances))
	}
	return &instance{&instances.Instances[0]}, nil
}

func (e *environ) StopInstances(insts []environs.Instance) error {
	if len(insts) == 0 {
		return nil
	}
	names := make([]string, len(insts))
	for i, inst := range insts {
		names[i] = inst.(*instance).InstanceId
	}
	_, err := e.ec2.TerminateInstances(names)
	return err
}

func (e *environ) Instances() ([]environs.Instance, error) {
	filter := ec2.NewFilter()
	filter.Add("instance-state-name", "pending", "running")
	filter.Add("group-name", e.groupName())

	resp, err := e.ec2.Instances(nil, filter)
	if err != nil {
		return nil, err
	}
	var insts []environs.Instance
	for i := range resp.Reservations {
		r := &resp.Reservations[i]
		for j := range r.Instances {
			insts = append(insts, &instance{&r.Instances[j]})
		}
	}
	return insts, nil
}

func (e *environ) Destroy() error {
	insts, err := e.Instances()
	if err != nil {
		return err
	}
	err = e.StopInstances(insts)
	if err != nil {
		return err
	}
	err = e.deleteState()
	if err != nil {
		return err
	}
	err = e.deleteSecurityGroups()
	if err != nil {
		return err
	}
	return nil
}


// delGroup deletes a security group, retrying if it is in use
// (something that will happen for quite a while after an
// environment has been destroyed)
func (e *environ) delGroup(g ec2.SecurityGroup) error {
	err := attempt("InvalidGroup.InUse", func() error {
		_, err := e.ec2.DeleteSecurityGroup(g)
		return err
	})
	if err != nil {
		return fmt.Errorf("cannot delete juju security group: %v", err)
	}
	return nil
}
	

func (e *environ) deleteSecurityGroups() error {
	// destroy security groups in parallel as we can have
	// many of them.
	p := newParallel(20)

	p.do(func() error {
		return e.delGroup(ec2.SecurityGroup{Name: e.groupName()})
	})

	resp, err := e.ec2.SecurityGroups(nil, nil)
	if err != nil {
		return fmt.Errorf("cannot list security groups: %v", err)
	}

	prefix := e.groupName() + "-"
	for _, g := range resp.Groups {
		if strings.HasPrefix(g.Name, prefix) {
			p.do(func() error {
				return e.delGroup(g.SecurityGroup)
			})
		}
	}
	
	return p.wait()
}
	

func (e *environ) machineGroupName(machineId int) string {
	return fmt.Sprintf("%s-%d", e.groupName(), machineId)
}

func (e *environ) groupName() string {
	return "juju-" + e.name
}

const retryDelay = time.Duration(2e9)

// attempt calls the given function until it does not return an error with
// the given ec2 error code.  This is to guard against ec2's "eventual
// consistency" semantics.
func attempt(code string, f func() error) (err error) {
	for i := 0; i < 20; i++ {
log.Printf("attempt %d", i)
		err = f()
log.Printf("error: %v", err)
		ec2err, _ := f().(*ec2.Error)
		if ec2err == nil || ec2err.Code != code {
			return
		}
		time.Sleep(5e9)
	}
log.Printf("number of attempts exceeded (err %v)", err)
	return
}

// setUpGroups creates the security groups for the new machine, and
// returns them.
// 
// Instances are tagged with a group so they can be distinguished from
// other instances that might be running on the same EC2 account.  In
// addition, a specific machine security group is created for each
// machine, so that its firewall rules can be configured per machine.
func (e *environ) setUpGroups(machineId int) ([]ec2.SecurityGroup, error) {
	jujuGroup := ec2.SecurityGroup{Name: e.groupName()}
	jujuMachineGroup := ec2.SecurityGroup{Name: e.machineGroupName(machineId)}

	f := ec2.NewFilter()
	f.Add("group-name", jujuGroup.Name, jujuMachineGroup.Name)
	groups, err := e.ec2.SecurityGroups(nil, f)
	if err != nil {
		return nil, fmt.Errorf("cannot get security groups: %v", err)
	}

	for _, g := range groups.Groups {
		switch g.Name {
		case jujuGroup.Name:
			jujuGroup = g.SecurityGroup
		case jujuMachineGroup.Name:
			jujuMachineGroup = g.SecurityGroup
		}
	}

	// Create the provider group if doesn't exist.
	if jujuGroup.Id == "" {
log.Printf("creating security group %q", jujuGroup.Name)
		r, err := e.ec2.CreateSecurityGroup(jujuGroup.Name, "juju group for "+e.name)
		if err != nil {
			return nil, fmt.Errorf("cannot create juju security group: %v", err)
		}
		jujuGroup = r.SecurityGroup
log.Printf("authorizing security group %v", jujuGroup)
		err = attempt("InvalidGroup.NotFound", func() error {
			_, err := e.ec2.AuthorizeSecurityGroup(jujuGroup, []ec2.IPPerm{
				// TODO delete this authorization when we can do
				// the zookeeper ssh tunnelling.
				{
					Protocol: "tcp",
					FromPort: zkPort,
					ToPort: zkPort,
					SourceIPs: []string{"0.0.0.0/0"},
				},
				{
					Protocol: "tcp",
					FromPort: 22,
					ToPort: 22,
					SourceIPs: []string{"0.0.0.0/0"},
				},
				// TODO authorize internal traffic
			})
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("cannot authorize security group: %v", err)
		}
	}

	// Create the machine-specific group, but first see if there's
	// one already existing from a previous machine launch;
	// if so, delete it, since it can have the wrong firewall setup
	if jujuMachineGroup.Id != "" {
		_, err := e.ec2.DeleteSecurityGroup(jujuMachineGroup)
		if err != nil {
			return nil, fmt.Errorf("cannot delete old security group %q: %v", jujuMachineGroup.Name, err)
		}
	}

	descr := fmt.Sprintf("juju group for %s machine %d", e.name, machineId)
	r, err := e.ec2.CreateSecurityGroup(jujuMachineGroup.Name, descr)
	if err != nil {
		return nil, fmt.Errorf("cannot create machine group %q: %v", jujuMachineGroup.Name, err)
	}

	return []ec2.SecurityGroup{jujuGroup, r.SecurityGroup}, nil
}
