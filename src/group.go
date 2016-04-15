/*******************************************************************************
*
* Copyright 2015 Stefan Majewsky <majewsky@gmx.net>
*
* This file is part of Holo.
*
* Holo is free software: you can redistribute it and/or modify it under the
* terms of the GNU General Public License as published by the Free Software
* Foundation, either version 3 of the License, or (at your option) any later
* version.
*
* Holo is distributed in the hope that it will be useful, but WITHOUT ANY
* WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR
* A PARTICULAR PURPOSE. See the GNU General Public License for more details.
*
* You should have received a copy of the GNU General Public License along with
* Holo. If not, see <http://www.gnu.org/licenses/>.
*
*******************************************************************************/

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

//GroupDefinition represents a UNIX group (as registered in /etc/group).
type GroupDefinition struct {
	Name   string `toml:"name"`             //the group name (the first field in /etc/group)
	GID    int    `toml:"gid,omitzero"`     //the GID (the third field in /etc/group), or 0 if no specific GID is enforced
	System bool   `toml:"system,omitempty"` //whether the group is a system group (this influences the GID selection if GID = 0)
}

//TypeName implements the EntityDefinition interface.
func (g *GroupDefinition) TypeName() string { return "group" }

//EntityID implements the EntityDefinition interface.
func (g *GroupDefinition) EntityID() string { return "group:" + g.Name }

//Attributes implements the EntityDefinition interface.
func (g *GroupDefinition) Attributes() string {
	var attrs []string
	if g.System {
		attrs = append(attrs, "type: system")
	}
	if g.GID > 0 {
		attrs = append(attrs, fmt.Sprintf("GID: %d", g.GID))
	}
	return strings.Join(attrs, ", ")
}

//WithSerializableState implements the EntityDefinition interface.
func (g *GroupDefinition) WithSerializableState(callback func(EntityDefinition)) {
	//we don't want to serialize the `system` attribute in diffs etc.
	system := g.System
	g.System = false
	callback(g)
	g.System = system
}

//Apply implements the EntityDefinition interface.
func (g *GroupDefinition) Apply(provisioned EntityDefinition) error {
	//assemble arguments
	var args []string
	if provisioned == nil && g.System {
		args = append(args, "--system")
	}
	if g.GID > 0 {
		args = append(args, "--gid", strconv.Itoa(g.GID))
	}
	args = append(args, g.Name)

	//call groupadd/groupmod
	command := "groupmod"
	if provisioned == nil {
		command = "groupadd"
	}
	err := ExecProgramOrMock(command, args...)
	if err != nil {
		return err
	}
	return AddProvisionedGroup(g.Name)
}

//Cleanup implements the EntityDefinition interface.
func (g *GroupDefinition) Cleanup() error {
	err := ExecProgramOrMock("groupdel", g.Name)
	if err != nil {
		return err
	}
	return RemoveProvisionedGroup(g.Name)
}

//Group implements the Entity interface for GroupDefinitions.
type Group struct {
	GroupDefinition
	DefinitionFiles []string //paths to the files defining this entity

	Orphaned bool //whether entity definition have been deleted (default: false)
	broken   bool //whether the entity definition is invalid (default: false)
}

//Definition implements the Entity interface.
func (g Group) Definition() EntityDefinition { return &g.GroupDefinition }

//IsOrphaned implements the Entity interface.
func (g Group) IsOrphaned() bool { return g.Orphaned }

//isValid is used inside the scanning algorithm to filter entities with
//broken definitions, which shall be skipped during `holo apply`.
func (g *Group) isValid() bool { return !g.broken }

//setInvalid is used inside the scnaning algorithm to mark entities with
//broken definitions, which shall be skipped during `holo apply`.
func (g *Group) setInvalid() { g.broken = true }

//PrintReport implements the Entity interface for Group.
func (g Group) PrintReport() {
	fmt.Printf("ENTITY: %s\n", g.EntityID())
	if g.Orphaned {
		fmt.Println("ACTION: Scrubbing (all definition files have been deleted)")
	} else {
		for _, defFile := range g.DefinitionFiles {
			fmt.Printf("found in: %s\n", defFile)
			fmt.Printf("SOURCE: %s\n", defFile)
		}
		if attributes := g.Attributes(); attributes != "" {
			fmt.Printf("with: %s\n", attributes)
		}
	}
}

type groupDiff struct {
	field    string
	actual   string
	expected string
}

//Apply performs the complete application algorithm for the given Entity.
//If the group does not exist yet, it is created. If it does exist, but some
//attributes do not match, it will be updated, but only if withForce is given.
func (g Group) Apply(withForce bool) (entityHasChanged bool) {
	//special handling for orphaned groups
	if g.Orphaned {
		return g.applyOrphaned(withForce)
	}

	//check if we have that group already
	actualDef, err := g.GetProvisionedState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "!! Cannot read group database: %s\n", err.Error())
		return false
	}

	//check if the actual properties diverge from our definition
	if actualDef != nil {
		actualGroup := actualDef.(*GroupDefinition)
		differences := []groupDiff{}
		if g.GID > 0 && g.GID != actualGroup.GID {
			differences = append(differences, groupDiff{"GID", strconv.Itoa(actualGroup.GID), strconv.Itoa(g.GID)})
		}

		if len(differences) != 0 {
			if withForce {
				for _, diff := range differences {
					fmt.Printf(">> fixing %s (was: %s)\n", diff.field, diff.actual)
				}
				err := g.GroupDefinition.Apply(actualGroup)
				if err != nil {
					fmt.Fprintf(os.Stderr, "!! %s\n", err.Error())
					return false
				}
				return true
			}
			_, err := os.NewFile(3, "file descriptor 3").Write([]byte("requires --force to overwrite\n"))
			if err != nil {
				fmt.Fprintf(os.Stderr, "!! %s\n", err.Error())
			}
		}
		return false
	}

	//create the group if it does not exist
	err = g.GroupDefinition.Apply(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "!! %s\n", err.Error())
		return false
	}
	return true
}

func (g Group) applyOrphaned(withForce bool) (entityHasChanged bool) {
	if !withForce {
		fmt.Fprintf(os.Stderr, "!! Won't do this without --force.\n")
		fmt.Fprintf(os.Stderr, ">> Call `holo apply --force group:%s` to delete this group.\n", g.Name)
		fmt.Fprintf(os.Stderr, ">> Or remove the group name from %s to keep the group.\n", RegistryPath())
		return false
	}

	//call groupdel and remove group from our registry
	err := g.GroupDefinition.Cleanup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "!! %s\n", err.Error())
		return false
	}
	return true
}

//GetProvisionedState implements the EntityDefinition interface.
func (g *GroupDefinition) GetProvisionedState() (EntityDefinition, error) {
	groupFile := GetPath("etc/group")

	//fetch entry from /etc/group
	fields, err := Getent(groupFile, func(fields []string) bool { return fields[0] == g.Name })
	if err != nil {
		return nil, err
	}
	//is there such a group?
	if fields == nil {
		return nil, nil
	}
	//is the group entry intact?
	if len(fields) < 4 {
		return nil, errors.New("invalid entry in /etc/group (not enough fields)")
	}

	//read fields in entry
	gid, err := strconv.Atoi(fields[2])
	return &GroupDefinition{
		Name: fields[0],
		GID:  gid,
	}, err
}
